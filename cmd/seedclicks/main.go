// seedclicks inserts synthetic ClickLog data into a GoLinx database for testing charts.
// Usage: go run ./cmd/seedclicks [path-to-db]
// Default DB: dev/golinx.db
package main

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	dbPath := "dev/golinx.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	defer db.Close()
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA busy_timeout=5000")

	// Get all link IDs
	rows, err := db.Query("SELECT ID, ShortName FROM Linx WHERE Type = 'link'")
	if err != nil {
		fmt.Fprintf(os.Stderr, "query links %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	type link struct {
		id   int64
		name string
	}
	var links []link
	for rows.Next() {
		var l link
		rows.Scan(&l.id, &l.name)
		links = append(links, l)
	}
	rows.Close()

	if len(links) == 0 {
		fmt.Println("No links found in database. Seed some links first.")
		os.Exit(0)
	}

	fmt.Printf("Found %d links. Generating 30 days of click data...\n", len(links))

	now := time.Now()
	total := 0

	tx, _ := db.Begin()
	stmt, _ := tx.Prepare("INSERT INTO ClickLog (LinxID, ClickedAt) VALUES (?, ?)")

	for _, l := range links {
		// Each link gets a random "popularity" weight
		weight := rand.Intn(20) + 1 // 1-20 clicks per day on average

		for day := 29; day >= 0; day-- {
			// Vary daily clicks: some days busy, some quiet
			dailyClicks := rand.Intn(weight*2) + 1
			// Weekends get fewer clicks
			dt := now.AddDate(0, 0, -day)
			if dt.Weekday() == time.Saturday || dt.Weekday() == time.Sunday {
				dailyClicks = dailyClicks / 3
				if dailyClicks == 0 {
					dailyClicks = 1
				}
			}

			for c := 0; c < dailyClicks; c++ {
				// Random time within the day (business hours weighted)
				hour := rand.Intn(10) + 8 // 8am-6pm
				minute := rand.Intn(60)
				second := rand.Intn(60)
				ts := time.Date(dt.Year(), dt.Month(), dt.Day(), hour, minute, second, 0, time.UTC).Unix()
				stmt.Exec(l.id, ts)
				total++
			}
		}
	}

	stmt.Close()
	tx.Commit()

	// Update ClickCount and LastClicked to match ClickLog totals
	db.Exec(`UPDATE Linx SET
		ClickCount = (SELECT COUNT(*) FROM ClickLog WHERE ClickLog.LinxID = Linx.ID),
		LastClicked = COALESCE((SELECT MAX(ClickedAt) FROM ClickLog WHERE ClickLog.LinxID = Linx.ID), 0)
		WHERE Type = 'link'`)

	fmt.Printf("Inserted %d click events across %d links over 30 days.\n", total, len(links))
	fmt.Println("ClickCount and LastClicked updated to match.")
}
