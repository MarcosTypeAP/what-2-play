package main

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"what2play/buildflags"

	_ "github.com/tursodatabase/go-libsql"
)

//go:embed web
var embedStaticFs embed.FS

//go:embed templates
var embedTemplatesFs embed.FS

var useMock = os.Getenv("MOCK") == "1"

func assert(cond bool, v ...any) {
	if cond {
		return
	}
	if len(v) > 0 {
		panic("assert error: " + fmt.Sprint(v...))
	}
	panic("assert error")
}

func assert400(w http.ResponseWriter, cond bool) bool {
	if cond {
		return false
	}
	blameUser(w, "assert")
	return true
}

func blameUser(w http.ResponseWriter, msg string) {
	w.Header().Add("HX-Redirect", "/server-error/user-fault?msg="+url.QueryEscape(msg))
	w.WriteHeader(http.StatusBadRequest)
}

func blameValve(w http.ResponseWriter) {
	w.Header().Add("HX-Redirect", "/server-error/valve-fault")
	w.WriteHeader(http.StatusServiceUnavailable)
}

func blameMyself(w http.ResponseWriter) {
	w.Header().Add("HX-Redirect", "/server-error")
	w.WriteHeader(http.StatusInternalServerError)
}

func getEnvRequired(key string) string {
	value, ok := os.LookupEnv(key)
	if !ok {
		slog.Error("missing $" + key)
		os.Exit(1)
	}
	return value
}

func prodToDevURL(parsedURL *url.URL) {
	parsedURL.Scheme = "http"
	parsedURL.Host = "127.0.0.1:8080"

	query := parsedURL.Query()
	query.Del("key")
	parsedURL.RawQuery = query.Encode()
}

func httpContextDo(ctx context.Context, method, url string, body io.Reader) (resp *http.Response, err error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, err
	}
	if useMock {
		prodToDevURL(req.URL)
	}
	return http.DefaultClient.Do(req)
}

func httpGet(reqURL string) (resp *http.Response, err error) {
	if useMock {
		parsedURL, err := url.Parse(reqURL)
		assert(err == nil, err, reqURL)

		prodToDevURL(parsedURL)

		reqURL = parsedURL.String()
	}
	return http.Get(reqURL)
}

const CookieSteamID = "steamid"

func getCookie(r *http.Request, name string) (*http.Cookie, error) {
	cookie, err := r.Cookie(name)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return nil, nil
		}
		return nil, err
	}
	err = cookie.Valid()
	if err != nil {
		return nil, nil
	}
	return cookie, nil
}

func renderTemplate(w http.ResponseWriter, templ *template.Template, status int, data any) error {
	buf := new(bytes.Buffer)
	err := templ.Execute(buf, data)
	assert(err == nil, err)

	w.WriteHeader(status)

	_, err = buf.WriteTo(w)
	if err != nil {
		return err
	}
	return nil
}

type Middleware = func(http.Handler) http.Handler

type contextKey byte

const steamIDKey contextKey = iota

func newSteamIDMiddleware() Middleware {
	return func(handler http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			steamIDCookie, err := getCookie(r, "steamid")
			if err != nil {
				slog.Error("get steamid cookie", "err", err)
				blameMyself(w)
				return
			}

			if steamIDCookie == nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return
			}

			ctx := context.WithValue(r.Context(), steamIDKey, steamIDCookie.Value)
			r = r.WithContext(ctx)

			handler.ServeHTTP(w, r)
		})
	}
}

func newThrottleMiddleware(requestsPerDay int) Middleware {
	requestsCountPerUser := make(map[string]int)

	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			parts := strings.SplitN(r.RemoteAddr, ":", 2)
			assert(len(parts) == 2, parts)
			ip := parts[0]

			if requestsCountPerUser[ip]+1 > requestsPerDay {
				_ = r.Body.Close()
				// TODO
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte("The limit for the number of requests per day has been reached\n"))
				return
			}
			requestsCountPerUser[ip]++

			h.ServeHTTP(w, r)
		})
	}
}

func newLatencyMiddleware(latency time.Duration) Middleware {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(latency)
			h.ServeHTTP(w, r)
		})
	}
}

func chainMiddlewares(handler http.Handler, middlewares ...Middleware) http.Handler {
	for _, m := range middlewares {
		handler = m(handler)
	}
	return handler
}

type Cache[K comparable, E any] struct {
	cache map[K]E
	mu    sync.RWMutex
}

func newCache[K comparable, E any]() Cache[K, E] {
	return Cache[K, E]{
		cache: make(map[K]E),
	}
}

func (c *Cache[K, E]) Get(key K) (value E, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	v, ok := c.cache[key]
	return v, ok
}

func (c *Cache[K, E]) Set(key K, value E) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cache == nil {
		return
	}
	c.cache[key] = value
}

type CacheGroup struct {
	games          Cache[string, map[int]SteamGame]
	prices         Cache[int, SteamGamePrice]
	usersInfo      Cache[string, SteamUserInfo]
	friends        Cache[string, []string]
	steamIDs       Cache[string, string]
	sortedGames    Cache[string, []SteamGame]
	gameCategories Cache[int, []int]
}

func newCacheGroup() *CacheGroup {
	return &CacheGroup{
		games:          newCache[string, map[int]SteamGame](),
		prices:         newCache[int, SteamGamePrice](),
		usersInfo:      newCache[string, SteamUserInfo](),
		friends:        newCache[string, []string](),
		steamIDs:       newCache[string, string](),
		sortedGames:    newCache[string, []SteamGame](),
		gameCategories: newCache[int, []int](),
	}
}

func getRoutes(steamAPIKey string, db *sql.DB, cache *CacheGroup) (http.Handler, error) {
	throttleMid := newThrottleMiddleware(120)
	steamIDMid := newSteamIDMiddleware()
	latencyMid := newLatencyMiddleware(500 * time.Millisecond)

	mux := http.NewServeMux()

	staticFs, err := fs.Sub(embedStaticFs, "web")
	assert(err == nil, err)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFs)))

	mux.Handle("GET /healthcheck", handleHealthCheck())

	mux.Handle("GET /{$}", chainMiddlewares(handleIndex(steamAPIKey, cache), throttleMid, steamIDMid))

	loginHandler := chainMiddlewares(handleLogin(steamAPIKey, cache), throttleMid)
	mux.Handle("GET /login", loginHandler)
	mux.Handle("POST /login", loginHandler)
	mux.Handle("POST /login/confirm", handleLoginConfirm())
	mux.Handle("GET /logout", handleLogout())

	mux.Handle("GET /games", chainMiddlewares(handleGames(steamAPIKey, db, cache), throttleMid, steamIDMid))

	mux.Handle("GET /server-error", handleServerErrorMyFault())
	mux.Handle("GET /server-error/valve-fault", handleServerErrorValveFault())
	mux.Handle("GET /server-error/user-fault", handleServerErrorUserFault())

	if buildflags.Dev {
		return latencyMid(mux), nil
	}
	return mux, nil
}

func loadEnvFile(file io.Reader) error {
	data, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	lines := bytes.Split(data, []byte{'\n'})
	for i, line := range lines {
		if len(line) == 0 || line[0] == '\n' || line[0] == '#' {
			continue
		}

		keyAndValue := bytes.SplitN(line, []byte{'='}, 2)
		if len(keyAndValue) < 2 {
			return fmt.Errorf("invalid key-value pair in line %d", i)
		}

		key := string(keyAndValue[0])
		value := string(keyAndValue[1])

		if len(key) == 0 || key[0] == ' ' || key[len(key)-1] == ' ' {
			return fmt.Errorf("invalid key in line %d: %q", i, key)
		}

		err := os.Setenv(key, value)
		if err != nil {
			return fmt.Errorf("set variable with key %q in line %d", key, i)
		}
	}

	return nil
}

func getTemplates(files ...string) *template.Template {
	templatesFs, err := fs.Sub(embedTemplatesFs, "templates")
	assert(err == nil, err)

	templs := template.Must(template.ParseFS(templatesFs, files...))
	templs = templs.Option("missingkey=error")

	return templs
}

func main() {
	switch strings.ToUpper(os.Getenv("LOG_LEVEL")) {
	case slog.LevelDebug.String():
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case slog.LevelInfo.String():
		slog.SetLogLoggerLevel(slog.LevelInfo)
	case slog.LevelWarn.String():
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case slog.LevelError.String():
		slog.SetLogLoggerLevel(slog.LevelError)
	}

	if buildflags.Dev {
		envFile, err := os.Open(".env")
		if err != nil {
			slog.Error("open .env file", "err", err)
			os.Exit(1)
		}

		err = loadEnvFile(envFile)
		_ = envFile.Close()
		if err != nil {
			slog.Error("load .env", "err", err)
			os.Exit(1)
		}
	}

	dbURL := getEnvRequired("DB_URL")
	dbToken := getEnvRequired("DB_TOKEN")

	if buildflags.Dev {
		dbURL = "file:./dev.db"
		dbToken = ""
	}

	db, err := NewDatabase(dbURL, dbToken)
	if err != nil {
		slog.Error("database", "err", err)
		os.Exit(1)
	}
	defer func() {
		_ = db.Close()
	}()

	port := getEnvRequired("PORT")
	steamAPIKey := getEnvRequired("STEAM_API_KEY")

	cache := newCacheGroup()

	mux, err := getRoutes(steamAPIKey, db, cache)
	if err != nil {
		slog.Error("routes", "err", err)
		os.Exit(1)
	}

	addr := "0.0.0.0:" + port
	slog.Info("listening on " + addr)
	err = http.ListenAndServe(addr, mux)
	if errors.Is(err, http.ErrServerClosed) {
		return
	}
	slog.Error("listen and serve", "err", err)
}
