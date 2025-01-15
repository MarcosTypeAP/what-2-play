package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"what2play/buildflags"
)

var multiplayerCategories = []int{
	1,  // Multi-player
	9,  // Co-op
	20, // MMO
	27, // Cross-Platform Multiplayer
	36, // Online PvP
	38, // Online Co-op
	49, // PvP
}

func handleHealthCheck() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.Body.Close()
		w.WriteHeader(http.StatusOK)
	})
}

func handleIndex(steamAPIKey string, cache *CacheGroup) http.Handler {
	templs := getTemplates("base.tmpl", "header.tmpl", "friends.tmpl")

	type Friend struct {
		SteamUserInfo
		Favorite bool
	}

	type Data struct {
		User    SteamUserInfo
		Friends []Friend
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()

		steamID := r.Context().Value(steamIDKey).(string)
		_ = steamID

		friendsSteamIDs, err := fetchSteamUserFriends(steamAPIKey, steamID, cache)
		if err != nil {
			slog.Error("fetch user friends", "steamid", steamID, "err", err)
			blameValve(w)
			return
		}

		usersInfo, err := fetchSteamUsersInfo(steamAPIKey, append(friendsSteamIDs, steamID), cache)
		if err != nil {
			slog.Error("fetch users info", "steamids", append(friendsSteamIDs, steamID), "err", err)
			blameValve(w)
			return
		}

		favoriteFriends := getFavoriteFriendsFromCookie(r, steamID)

		var userInfo SteamUserInfo
		friends := make([]Friend, 0, len(friendsSteamIDs))
		for _, info := range usersInfo {
			if info.SteamID == steamID {
				userInfo = info
				continue
			}

			_, favorite := favoriteFriends[info.SteamID]

			friends = append(friends, Friend{
				SteamUserInfo: info,
				Favorite:      favorite,
			})
		}

		data := Data{
			User:    userInfo,
			Friends: friends,
		}
		err = renderTemplate(w, templs.Lookup("friends.tmpl"), http.StatusOK, data)
		if err != nil {
			slog.Error("send friends template", "err", err)
			return
		}
	})
}

func getFavoriteFriendsFromCookie(r *http.Request, steamID string) map[string]struct{} {
	cookie, err := r.Cookie("favorite-friends-" + steamID)
	if err != nil {
		return nil
	}

	err = cookie.Valid()
	if err != nil {
		return nil
	}

	ids := strings.Split(cookie.Value, ",")
	if len(ids) == 0 {
		return nil
	}

	favoriteFriends := make(map[string]struct{}, len(ids))

	for _, id := range ids {
		if len(id) == 0 {
			continue
		}
		favoriteFriends[id] = struct{}{}
	}

	return favoriteFriends
}

func handleGames(steamAPIKey string, db *sql.DB, cache *CacheGroup) http.Handler {
	templs := getTemplates("base.tmpl", "header.tmpl", "games.tmpl")

	type Data struct {
		User        SteamUserInfo
		Games       []SteamGame
		NextPageURL string
		DevMode     bool
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()

		steamID := r.Context().Value(steamIDKey).(string)
		_ = steamID

		friends, ok := r.URL.Query()["steamid"]
		if !ok {
			blameUser(w, "missing steamid query param")
			return
		}
		for _, id := range friends {
			if len(id) == 0 {
				blameUser(w, "invalid steamid query param")
				return
			}
		}

		pageStr := r.URL.Query().Get("page")
		if len(pageStr) == 0 {
			blameUser(w, "invalid page query param")
			return
		}
		page, err := strconv.ParseInt(pageStr, 10, 16)
		if err != nil {
			blameUser(w, "invalid page query param")
			return
		}

		_usersInfo, err := fetchSteamUsersInfo(steamAPIKey, []string{steamID}, cache)
		if err != nil || len(_usersInfo) == 0 {
			slog.Error("fetch user info", "steamid", steamID, "err", err)
			blameValve(w)
			return
		}
		userInfo := _usersInfo[0]

		users := append(friends, steamID)

		sortedGames, err := getSteamSortedGames(steamAPIKey, steamID, users, cache)
		if err != nil {
			slog.Error("get sorted games", "steamids", users, "err", err)
			blameValve(w)
			return
		}

		const gamesPerPage = 20

		finalGames := make([]SteamGame, 0, gamesPerPage)
		skipped := 0
		offset := gamesPerPage * int(page)

		categoriesIter := newFetchGameCategoriesIter(sortedGames, gamesPerPage, db, cache)

		for gameAndCategories, err := range categoriesIter {
			if err != nil {
				slog.Error("fetch game categories", "err", err)
				blameValve(w)
				return
			}

			if len(finalGames) == gamesPerPage {
				break
			}

			isMultiplayer := false
			for _, category := range gameAndCategories.categories {
				if slices.Contains(multiplayerCategories, category) {
					isMultiplayer = true
					break
				}
			}

			if !isMultiplayer {
				continue
			}

			if skipped < offset {
				skipped++
				continue
			}
			finalGames = append(finalGames, gameAndCategories.game)
		}

		data := Data{
			User:    userInfo,
			Games:   finalGames,
			DevMode: buildflags.Dev,
		}
		if len(finalGames) > 0 {
			queryParams := r.URL.Query()
			queryParams.Set("page", fmt.Sprint(page+1))

			nextPageURL := *r.URL
			nextPageURL.RawQuery = queryParams.Encode()

			data.NextPageURL = nextPageURL.String()
		}

		if page == 0 {
			err = renderTemplate(w, templs.Lookup("games.tmpl"), http.StatusOK, data)
			if err != nil {
				slog.Error("send games.tmpl template", "err", err)
				return
			}
			return
		}

		err = renderTemplate(w, templs.Lookup("games-page"), http.StatusOK, data)
		if err != nil {
			slog.Error("send games-page template", "err", err)
			return
		}
	})
}

func handleLogin(steamAPIKey string, cache *CacheGroup) http.Handler {
	templs := getTemplates("base.tmpl", "login.tmpl")

	type Data struct {
		Confirm bool
		Profile struct {
			Name    string
			Picture string
		}
		Fields struct {
			Identifier struct {
				Value string
				Error string
			}
		}
	}

	validateData := func(fields url.Values) (data Data, valid bool) {
		f := &data.Fields

		identifier := strings.TrimSpace(fields.Get("identifier"))
		if len(identifier) == 0 {
			f.Identifier.Error = "Field required"
		} else if strings.Contains(identifier, " ") {
			f.Identifier.Error = "Field must not have spaces"
		}
		f.Identifier.Value = identifier

		valid = len(f.Identifier.Error) == 0

		return data, valid
	}

	looksLikeSteamID := func(identifier string) bool {
		for _, ch := range identifier {
			if ch < '0' || ch > '9' {
				return false
			}
		}
		return true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()

		if r.Method == http.MethodGet {
			err := renderTemplate(w, templs.Lookup("login.tmpl"), http.StatusOK, Data{})
			if err != nil {
				slog.Error("send login template", "err", err)
				blameMyself(w)
				return
			}
			return
		}

		assert(r.Method == http.MethodPost, r.Method)

		err := r.ParseForm()
		if err != nil {
			blameUser(w, "invalid form data")
			return
		}

		data, valid := validateData(r.Form)
		if !valid {
			err = renderTemplate(w, templs.Lookup("login"), http.StatusOK, data)
			if err != nil {
				slog.Error("send login template", "err", err)
				blameMyself(w)
				return
			}
			return
		}

		var userInfo SteamUserInfo

		if looksLikeSteamID(data.Fields.Identifier.Value) {
			steamID := data.Fields.Identifier.Value

			usersInfo, err := fetchSteamUsersInfo(steamAPIKey, []string{steamID}, cache)
			if err != nil {
				slog.Error("fetch user info", "steamid", steamID, "err", err)
				blameValve(w)
				return
			}
			if len(usersInfo) == 0 {
				// maybe it was not
				steamID, err = fetchSteamID(steamAPIKey, data.Fields.Identifier.Value)
				if err != nil {
					slog.Error("fetch steamid", "identifier", data.Fields.Identifier.Value, "err", err)
					blameValve(w)
					return
				}
				if len(steamID) == 0 {
					data.Fields.Identifier.Error = "Not found"

					err = renderTemplate(w, templs.Lookup("login"), http.StatusOK, data)
					if err != nil {
						slog.Error("send login template (username not found)", "err", err)
						blameMyself(w)
						return
					}
					return
				}

				usersInfo, err = fetchSteamUsersInfo(steamAPIKey, []string{steamID}, cache)
				if err != nil || len(usersInfo) == 0 {
					slog.Error("fetch user info", "steamid", steamID, "err", err)
					blameValve(w)
					return
				}
			}
			userInfo = usersInfo[0]
		} else {
			steamID, err := fetchSteamID(steamAPIKey, data.Fields.Identifier.Value)
			if err != nil {
				slog.Error("fetch steamid", "identifier", data.Fields.Identifier.Value, "err", err)
				blameValve(w)
				return
			}
			if len(steamID) == 0 {
				data.Fields.Identifier.Error = "Not found"

				err = renderTemplate(w, templs.Lookup("login"), http.StatusOK, data)
				if err != nil {
					slog.Error("send login template (username not found)", "err", err)
					blameMyself(w)
					return
				}
				return
			}

			usersInfo, err := fetchSteamUsersInfo(steamAPIKey, []string{steamID}, cache)
			if err != nil || len(usersInfo) == 0 {
				slog.Error("fetch user info", "steamid", steamID, "err", err)
				blameValve(w)
				return
			}
			userInfo = usersInfo[0]
		}

		renderTemplate(w, templs.Lookup("confirm-user"), http.StatusOK, userInfo)
	})
}

func handleLoginConfirm() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		assert(r.Method == http.MethodPost, r.Method)

		steamID := r.URL.Query().Get("steamid")
		if len(steamID) == 0 {
			blameUser(w, "invalid steamid query param")
			return
		}

		cookie := http.Cookie{
			Name:     CookieSteamID,
			Value:    steamID,
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Path:     "/",
			MaxAge:   int((time.Hour * 24 * 365).Seconds()),
		}
		http.SetCookie(w, &cookie)

		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}

func handleLogout() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			_ = r.Body.Close()
		}()
		assert(r.Method == http.MethodGet, r.Method)

		cookie := http.Cookie{
			Name:     CookieSteamID,
			SameSite: http.SameSiteStrictMode,
			MaxAge:   -1,
		}
		http.SetCookie(w, &cookie)

		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

func handleServerErrorMyFault() http.Handler {
	templs := getTemplates("server-error.tmpl")

	type Data struct {
		Error string
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := Data{
			Error: "It seems we have some problems back here on the server.",
		}

		err := renderTemplate(w, templs, http.StatusOK, data)
		if err != nil {
			slog.Error("send server error template (my fault)", "err", err)
			return
		}
	})
}

func handleServerErrorValveFault() http.Handler {
	templs := getTemplates("server-error.tmpl")

	type Data struct {
		Error string
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := Data{
			Error: "It appears that the Steam API is not working properly.",
		}

		err := renderTemplate(w, templs, http.StatusOK, data)
		if err != nil {
			slog.Error("send server error template (valve fault)", "err", err)
			return
		}
	})
}

func handleServerErrorUserFault() http.Handler {
	templs := getTemplates("server-error.tmpl")

	type Data struct {
		Error string
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		msg := r.URL.Query().Get("msg")

		data := Data{}
		if len(msg) > 0 {
			data.Error = "There is something wrong with your client: " + msg + "."
		} else {
			data.Error = "There is something wrong with your client."
		}

		err := renderTemplate(w, templs, http.StatusOK, data)
		if err != nil {
			slog.Error("send server error template (user fault)", "err", err)
			return
		}
	})
}
