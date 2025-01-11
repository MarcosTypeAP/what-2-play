package main

import (
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	_ "github.com/tursodatabase/go-libsql"
)

func NewDatabase(url, token string) (*sql.DB, error) {
	if len(token) > 0 {
		url += "?authToken=" + token
	}

	db, err := sql.Open("libsql", url)
	if err != nil {
		return nil, err
	}
	return db, nil
}

func decodeDBGameCategories(encoded []byte) []int {
	assert(len(encoded) >= 2)

	numberOfCategories := binary.LittleEndian.Uint16(encoded)
	encoded = encoded[2:]

	if len(encoded) == 0 {
		return nil
	}

	categories := make([]int, numberOfCategories)

	for i := range numberOfCategories {
		categories[i] = int(binary.LittleEndian.Uint16(encoded))
		encoded = encoded[2:]
	}

	return categories
}

func encodeDBGameCategories(categories []int) []byte {
	assert(len(categories) < math.MaxUint16, len(categories))

	encoded := make([]byte, 2+len(categories)*2) // count + items
	w := encoded

	binary.LittleEndian.PutUint16(w, uint16(len(categories)))
	w = w[2:]

	for _, category := range categories {
		assert(category < math.MaxUint16, category)

		binary.LittleEndian.PutUint16(w, uint16(category))
		w = w[2:]
	}

	return encoded
}

func queryGameCategories(db *sql.DB, appIDs []int) (map[int][]int, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.Prepare("SELECT categories FROM game_categories WHERE appid = ?")
	assert(err == nil, err)

	categoriesPerGame := make(map[int][]int, len(appIDs))

	for _, appID := range appIDs {
		var encodedCategories []byte

		if err := stmt.QueryRow(appID).Scan(&encodedCategories); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			return nil, fmt.Errorf("execute query (appid=%d): %w", appID, err)
		}

		categoriesPerGame[appID] = decodeDBGameCategories(encodedCategories)
	}

	err = tx.Commit()
	if err != nil {
		return nil, fmt.Errorf("commit transaction: %v", err)
	}

	return categoriesPerGame, nil
}

func saveGameCategories(db *sql.DB, categoriesPerGame map[int][]int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %v", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	stmt, err := tx.Prepare("INSERT INTO game_categories (appid, categories) VALUES (?, ?)")
	assert(err == nil, err)

	for appID, categories := range categoriesPerGame {
		_, err = stmt.Exec(appID, encodeDBGameCategories(categories))
		if err != nil {
			return fmt.Errorf("exec: %v", err)
		}
	}

	err = tx.Commit()
	if err != nil {
		return fmt.Errorf("commit transaction: %v", err)
	}

	return nil
}
