package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sync/errgroup"
)

var ErrItemNotFound = errors.New("item not found")

type SteamUserInfo struct {
	SteamID    string
	Username   string
	PictureURL string
}

func fetchSteamUsersInfo(steamAPIKey string, steamIDs []string, cache *CacheGroup) ([]SteamUserInfo, error) {
	usersInfo := make([]SteamUserInfo, 0, len(steamIDs))
	steamIDsStr := strings.Builder{}

	for _, steamID := range steamIDs {
		if userInfo, ok := cache.usersInfo.Get(steamID); ok {
			usersInfo = append(usersInfo, userInfo)
			continue
		}
		steamIDsStr.WriteString(steamID)
		steamIDsStr.WriteByte(',')
	}

	if len(usersInfo) == len(steamIDs) {
		slog.Debug("fetchSteamUserInfo: full cache hit", "count", len(usersInfo))
		return usersInfo, nil
	}
	if len(usersInfo) > 0 {
		slog.Debug("fetchSteamUserInfo: partial cache hit", "count", len(usersInfo))
	}

	type SteamResponse struct {
		Response struct {
			Players []struct {
				SteamID    string `json:"steamid"`
				Username   string `json:"personaname"`
				PictureURL string `json:"avatarfull"`
			} `json:"players"`
		} `json:"response"`
	}

	const URL = "https://api.steampowered.com/ISteamUser/GetPlayerSummaries/v0002?key=%s&steamids=%s"

	res, err := httpGet(fmt.Sprintf(URL, steamAPIKey, steamIDsStr.String()))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	assert(err == nil, err)

	for _, userInfo := range steamRes.Response.Players {
		usersInfo = append(usersInfo, SteamUserInfo(userInfo))
		cache.usersInfo.Set(userInfo.SteamID, SteamUserInfo(userInfo))
	}

	return usersInfo, nil
}

type SteamGame struct {
	AppID           int
	Playtime2Weeks  int
	PlaytimeForever int
	Name            string
	Free            bool
}

func fetchSteamUserOwnedGames(steamAPIKey, steamID string, cache *CacheGroup) (map[int]SteamGame, error) {
	if games, ok := cache.games.Get(steamID); ok {
		slog.Debug("fetchSteamUserOwnedGames: cache hit", "steamid", steamID)
		return games, nil
	}

	type SteamResponse struct {
		Response struct {
			Games []struct {
				AppID           int    `json:"appid"`
				Name            string `json:"name"`
				Playtime2Weeks  int    `json:"playtime_2weeks"`
				PlaytimeForever int    `json:"playtime_forever"`
			} `json:"games"`
		} `json:"response"`
	}

	const URL = "https://api.steampowered.com/IPlayerService/GetOwnedGames/v0001?key=%s&steamid=%s&include_appinfo=true&include_played_free_games=true"

	res, err := httpGet(fmt.Sprintf(URL, steamAPIKey, steamID))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	assert(err == nil, err)

	appIDs := make([]int, len(steamRes.Response.Games))
	for i, g := range steamRes.Response.Games {
		appIDs[i] = g.AppID
	}

	prices, err := fetchSteamGamesPrices(appIDs, cache)
	if err != nil {
		return nil, fmt.Errorf("fetch game prices: %v", err)
	}

	games := make(map[int]SteamGame, len(steamRes.Response.Games))
	for _, g := range steamRes.Response.Games {
		games[g.AppID] = SteamGame{
			AppID:           g.AppID,
			Name:            g.Name,
			Free:            prices[g.AppID].Free(),
			Playtime2Weeks:  g.Playtime2Weeks,
			PlaytimeForever: g.PlaytimeForever,
		}
	}

	cache.games.Set(steamID, games)
	return games, nil
}

type SteamGamePrice struct {
	Currency        string
	Initial         int
	Final           int
	DiscountPercent int
}

func (p SteamGamePrice) Free() bool {
	return p.Initial == 0
}

type _fetchSteamPriceData struct {
	value struct {
		PriceOverview struct {
			Currency        string `json:"currency"`
			Initial         int    `json:"initial"`
			Final           int    `json:"final"`
			DiscountPercent int    `json:"discount_percent"`
		} `json:"price_overview"`
	}
}

func (d *_fetchSteamPriceData) UnmarshalJSON(data []byte) error {
	// the "data" key can be an object or an empty array.
	// javascript, you wouldn't understand it
	if err := json.Unmarshal(data, &[]any{}); err == nil {
		return nil
	}
	return json.Unmarshal(data, &d.value)
}

func fetchSteamGamesPrices(appIDs []int, cache *CacheGroup) (map[int]SteamGamePrice, error) {
	prices := make(map[int]SteamGamePrice, len(appIDs))

	type SteamResponse = map[string]struct {
		Success bool                 `json:"success"`
		Data    _fetchSteamPriceData `json:"data"`
	}

	const URL = "https://store.steampowered.com/api/appdetails?appids=%s&filters=price_overview"

	appIDsArg := strings.Builder{}
	for _, id := range appIDs {
		if price, ok := cache.prices.Get(id); ok {
			prices[id] = price
			continue
		}
		appIDsArg.WriteString(fmt.Sprint(id))
		appIDsArg.WriteByte(',')
	}

	if len(prices) == len(appIDs) {
		slog.Debug("fetchSteamGamesPrices: full cache hit", "count", len(prices))
		return prices, nil
	}
	if len(prices) > 0 {
		slog.Debug("fetchSteamGamesPrices: partial cache hit", "count", len(prices))
	}

	gamesLeftCount := len(appIDs) - len(prices)

	res, err := httpGet(fmt.Sprintf(URL, appIDsArg.String()))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.Body.Close()
	}()
	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	assert(err == nil, err)

	for appIDStr, data := range steamRes {
		if !data.Success {
			continue
		}
		appID, err := strconv.ParseInt(appIDStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse appid: %v", err)
		}

		price := SteamGamePrice(data.Data.value.PriceOverview)
		prices[int(appID)] = price

		cache.prices.Set(int(appID), price)
	}

	slog.Debug("fetchSteamGamesPrices: fetched from steam api", "count", gamesLeftCount)

	return prices, nil
}

func fetchSteamUserFriends(steamAPIKey, steamID string, cache *CacheGroup) ([]string, error) {
	if friends, ok := cache.friends.Get(steamID); ok {
		slog.Debug("fetchSteamUserFriends: cache hit", "steamid", steamID)
		return friends, nil
	}

	type SteamResponse struct {
		Friendslist struct {
			Friends []struct {
				SteamID string `json:"steamid"`
			} `json:"friends"`
		} `json:"friendslist"`
	}

	const URL = "https://api.steampowered.com/ISteamUser/GetFriendList/v0001?key=%s&steamid=%s&relationship=friend"

	res, err := httpGet(fmt.Sprintf(URL, steamAPIKey, steamID))
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	assert(err == nil, err)

	friends := make([]string, len(steamRes.Friendslist.Friends))
	for i, f := range steamRes.Friendslist.Friends {
		friends[i] = f.SteamID
	}

	cache.friends.Set(steamID, friends)
	return friends, nil
}

func fetchSteamID(steamAPIKey, username string) (string, error) {
	type SuccessCode byte

	const (
		SuccessMatch   SuccessCode = 1
		SuccessNoMatch SuccessCode = 42
	)

	type SteamResponse struct {
		Response struct {
			SteamID string      `json:"steamid"`
			Success SuccessCode `json:"success"`
			Message string      `json:"message"`
		} `json:"response"`
	}

	const URL = "https://api.steampowered.com/ISteamUser/ResolveVanityURL/v0001?key=%s&vanityurl=%s"

	res, err := httpGet(fmt.Sprintf(URL, steamAPIKey, username))
	if err != nil {
		return "", err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	assert(err == nil, err)

	switch steamRes.Response.Success {
	case SuccessMatch:
		return steamRes.Response.SteamID, nil

	case SuccessNoMatch:
		return "", nil

	default:
		panic(fmt.Errorf("invalid success code: %d", steamRes.Response.Success))
	}
}

func fetchSteamGameCategories(ctx context.Context, appID int, cache *CacheGroup) ([]int, error) {
	if categories, ok := cache.gameCategories.Get(appID); ok {
		slog.Debug("fetchSteamGameCategories: cache hit", "appid", appID)
		return categories, nil
	}

	type SteamResponse map[string]struct {
		Success bool `json:"success"`
		Data    struct {
			Categories []struct {
				ID int `json:"id"`
			} `json:"categories"`
		} `json:"data"`
	}

	const URL = "https://store.steampowered.com/api/appdetails?appids=%d&filters=categories"

	res, err := httpContextDo(ctx, http.MethodGet, fmt.Sprintf(URL, appID), nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = res.Body.Close()
	}()

	decoder := json.NewDecoder(res.Body)
	var steamRes SteamResponse
	err = decoder.Decode(&steamRes)
	if err != nil {
		err, ok := err.(*json.UnmarshalTypeError)
		assert(ok, err)

		if err.Value == "array" && err.Field == "data" {
			cache.gameCategories.Set(appID, nil)
			return nil, nil
		}
		assert(true, err)
	}

	gameRes, ok := steamRes[fmt.Sprint(appID)]
	if !ok {
		cache.gameCategories.Set(appID, nil)
		return nil, nil
	}

	if !gameRes.Success {
		cache.gameCategories.Set(appID, nil)
		return nil, nil
	}

	categories := make([]int, len(gameRes.Data.Categories))
	for i, category := range gameRes.Data.Categories {
		categories[i] = category.ID
	}

	cache.gameCategories.Set(appID, categories)

	return categories, nil
}

func fetchGamesCategories(appIDs []int, dst map[int][]int, db *sql.DB, cache *CacheGroup) error {
	queryAppIDs := make([]int, 0, len(appIDs))

	for _, appID := range appIDs {
		if categories, ok := cache.gameCategories.Get(appID); ok {
			dst[appID] = categories
			continue
		}
		queryAppIDs = append(queryAppIDs, appID)
	}

	if len(queryAppIDs) == 0 {
		slog.Debug("fetchGamesCategories: full local cache hit", "count", len(appIDs))
		return nil
	}
	if len(appIDs)-len(queryAppIDs) > 0 {
		slog.Debug("fetchGamesCategories: partial local cache hit", "count", len(appIDs)-len(queryAppIDs))
	}

	categoriesFromDB, err := queryGameCategories(db, queryAppIDs)
	if err != nil {
		return fmt.Errorf("query from db: %v", err)
	}

	gamesLeft := queryAppIDs

	queryAppIDs = make([]int, 0, len(gamesLeft))

	for _, appID := range gamesLeft {
		if categories, ok := categoriesFromDB[appID]; ok {
			cache.gameCategories.Set(appID, categories)
			dst[appID] = categories
			continue
		}
		queryAppIDs = append(queryAppIDs, appID)
	}

	if len(queryAppIDs) == 0 {
		slog.Debug("fetchGamesCategories: full db cache hit", "count", len(appIDs))
		return nil
	}
	if len(gamesLeft)-len(queryAppIDs) > 0 {
		slog.Debug("fetchGamesCategories: partial db cache hit", "count", len(gamesLeft)-len(queryAppIDs))
	}

	newCategories := make(map[int][]int, len(queryAppIDs))

	// Test if the steam API limit was reached
	testAppID := queryAppIDs[len(queryAppIDs)-1]
	queryAppIDs = queryAppIDs[:len(queryAppIDs)-1]

	categories, err := fetchSteamGameCategories(context.Background(), testAppID, cache)
	if err != nil {
		return fmt.Errorf("fetch steam game categories (appid=%d): steam api limit probably reached: %v", testAppID, err)
	} else {
		newCategories[testAppID] = categories
		dst[testAppID] = categories
	}

	// Fetch the rest
	eg, ctx := errgroup.WithContext(context.Background())
	eg.SetLimit(10)

	for _, appID := range queryAppIDs {
		eg.Go(func() error {
			categories, err := fetchSteamGameCategories(ctx, appID, cache)
			if err != nil {
				return fmt.Errorf("appid %d: %v", appID, err)
			}
			newCategories[appID] = categories
			dst[appID] = categories
			return nil
		})
	}

	fetchErr := eg.Wait()

	assert(len(newCategories) > 0)
	slog.Debug("fetchGamesCategories: fetched games from steam api", "count", len(newCategories))

	err = saveGameCategories(db, newCategories)
	if err != nil {
		return fmt.Errorf("save new game categories to database: %v", err)
	}

	if fetchErr != nil {
		return fmt.Errorf("fetch steam game categories: %v", fetchErr)
	}
	return nil
}

func newFetchGameCategoriesIter(games []SteamGame, gamesPerPage int, db *sql.DB, cache *CacheGroup) iter.Seq2[struct {
	game       SteamGame
	categories []int
}, error] {
	type YieldValue = struct {
		game       SteamGame
		categories []int
	}

	categoriesPerGame := make(map[int][]int, len(games))

	return func(yield func(YieldValue, error) bool) {
		for i, game := range games {

			if i == len(categoriesPerGame) {
				appIDs := make([]int, min(gamesPerPage, len(games)-i))

				for j := range gamesPerPage {
					if i+j >= len(games) {
						break
					}
					appIDs[j] = games[i+j].AppID
				}

				err := fetchGamesCategories(appIDs, categoriesPerGame, db, cache)
				if err != nil {
					yield(YieldValue{}, err)
					return
				}
			}

			categories, ok := categoriesPerGame[game.AppID]
			if !ok {
				continue
			}

			if !yield(YieldValue{game, categories}, nil) {
				return
			}
		}
	}
}

func getSteamSortedGames(steamAPIKey string, steamID string, users []string, cache *CacheGroup) ([]SteamGame, error) {
	sortedUsers := slices.Sorted(slices.Values(users))
	cacheKey := strings.Join(sortedUsers, ",")

	if sortedGames, ok := cache.sortedGames.Get(cacheKey); ok {
		slog.Debug("handleGames: cache hit", "users", cacheKey)
		return sortedGames, nil
	}

	usersGames := make(map[string]map[int]SteamGame, len(users))
	for _, id := range users {
		games, err := fetchSteamUserOwnedGames(steamAPIKey, id, cache)
		if err != nil {
			return nil, fmt.Errorf("fetch user owned games (steamid=%s): %v", id, err)
		}
		usersGames[id] = games
	}

	filteredGames := make(map[int]SteamGame, 32) // if you are using this, you have many games

	for appID, game := range usersGames[steamID] {
		if game.Free {
			filteredGames[appID] = game
			continue
		}

		everyoneHasIt := true
		for userID, games := range usersGames {
			if userID == steamID {
				continue
			}
			if _, ok := games[appID]; !ok {
				everyoneHasIt = false
				break
			}
		}

		if everyoneHasIt {
			filteredGames[appID] = game
		}
	}
	for userID, games := range usersGames {
		if userID == steamID {
			continue
		}
		for appID, game := range games {
			if !game.Free {
				continue
			}
			if _, exists := filteredGames[appID]; exists {
				continue
			}
			filteredGames[appID] = game
		}
	}

	sortedGames := make([]SteamGame, 0, len(filteredGames))
	for _, game := range filteredGames {
		sortedGames = append(sortedGames, game)
	}
	slices.SortFunc(sortedGames, func(a, b SteamGame) int {
		if a.Playtime2Weeks > 0 || b.Playtime2Weeks > 0 {
			switch {
			case a.Playtime2Weeks > b.Playtime2Weeks:
				return -1
			case a.Playtime2Weeks < b.Playtime2Weeks:
				return 1
			case a.Name > b.Name:
				return -1
			}
			return 1
		}

		switch {
		case a.PlaytimeForever > b.PlaytimeForever:
			return -1
		case a.PlaytimeForever < b.PlaytimeForever:
			return 1
		case a.Name > b.Name:
			return -1
		}
		return 1
	})

	cache.sortedGames.Set(cacheKey, sortedGames)

	return sortedGames, nil
}
