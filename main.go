package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"database/sql"
	_ "modernc.org/sqlite"
)

type archive struct {
	db   *sql.DB
	root string
}

type parserRecord struct {
	Summary struct {
		ID              int             `json:"id"`
		StartDate       string          `json:"startDate"`
		StartTime       string          `json:"startTime"`
		HomeAndAwayTeam json.RawMessage `json:"homeAndAwayTeam"`
		MatchMaps       json.RawMessage `json:"matchMaps"`
	} `json:"summary"`
	Overview json.RawMessage `json:"overview"`
	Demos    []struct {
		CreatedAt   string `json:"createdAt"`
		Map         string `json:"map"`
		DownloadURL string `json:"downloadUrl"`
		Player      string `json:"player"`
		PlayerID    int    `json:"playerId"`
	} `json:"demos"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	dbPath := env("DB_PATH", "/data/fps-demo.db")
	root := env("DEMO_ROOT", "/data/demos")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	a := &archive{db: db, root: root}
	if err := a.init(); err != nil {
		log.Fatal(err)
	}
	switch os.Args[1] {
	case "serve":
		a.serve()
	case "import":
		fs := flag.NewFlagSet("import", flag.ExitOnError)
		input := fs.String("file", "matches.json", "parser matches.json")
		_ = fs.Parse(os.Args[2:])
		if err := a.importFile(*input); err != nil {
			log.Fatal(err)
		}
	case "sync":
		if err := a.sync(); err != nil {
			log.Fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "fps-demo: CoD2 demo archive\n\nUsage: fps-demo serve | import -file matches.json | sync")
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func (a *archive) init() error {
	if err := os.MkdirAll(a.root, 0750); err != nil {
		return err
	}
	_, err := a.db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS matches (id INTEGER PRIMARY KEY, played_at TEXT, home_team TEXT, away_team TEXT, raw_json TEXT NOT NULL, imported_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS demos (id INTEGER PRIMARY KEY AUTOINCREMENT, match_id INTEGER NOT NULL REFERENCES matches(id) ON DELETE CASCADE, player TEXT, player_id INTEGER, map TEXT, created_at TEXT, source_url TEXT NOT NULL, file_path TEXT, size_bytes INTEGER NOT NULL DEFAULT 0, sha256 TEXT, downloaded_at TEXT, UNIQUE(match_id, source_url));
CREATE INDEX IF NOT EXISTS demos_map ON demos(map); CREATE INDEX IF NOT EXISTS demos_player ON demos(player); CREATE INDEX IF NOT EXISTS matches_played_at ON matches(played_at);`)
	return err
}

func (a *archive) importFile(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var records []parserRecord
	if err := json.Unmarshal(b, &records); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range records {
		if r.Summary.ID == 0 {
			continue
		}
		played := strings.TrimSpace(r.Summary.StartDate + " " + r.Summary.StartTime)
		home, away := teamNames(r.Summary.HomeAndAwayTeam)
		if _, err := tx.Exec(`INSERT INTO matches(id,played_at,home_team,away_team,raw_json,imported_at) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET played_at=excluded.played_at,home_team=excluded.home_team,away_team=excluded.away_team,raw_json=excluded.raw_json,imported_at=excluded.imported_at`, r.Summary.ID, played, home, away, string(mustJSON(r)), time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		for _, d := range r.Demos {
			if d.DownloadURL == "" {
				continue
			}
			_, err := tx.Exec(`INSERT INTO demos(match_id,player,player_id,map,created_at,source_url) VALUES(?,?,?,?,?,?) ON CONFLICT(match_id,source_url) DO UPDATE SET player=excluded.player,player_id=excluded.player_id,map=excluded.map,created_at=excluded.created_at`, r.Summary.ID, d.Player, d.PlayerID, d.Map, d.CreatedAt, d.DownloadURL)
			if err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("imported %d matches", len(records))
	return a.downloadMissing(context.Background())
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func teamNames(raw json.RawMessage) (string, string) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return "", ""
	}
	var names []string
	var walk func(any)
	walk = func(x any) {
		switch y := x.(type) {
		case map[string]any:
			for k, v := range y {
				lk := strings.ToLower(k)
				if lk == "name" || lk == "teamname" {
					if s, ok := v.(string); ok && s != "" {
						names = append(names, s)
					}
				}
				walk(v)
			}
		case []any:
			for _, v := range y {
				walk(v)
			}
		}
	}
	walk(v)
	out := []string{}
	seen := map[string]bool{}
	for _, n := range names {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	if len(out) > 2 {
		out = out[:2]
	}
	return strings.Join(out, " / "), ""
}

func (a *archive) sync() error {
	command := env("PARSER_COMMAND", "")
	if command == "" {
		return errors.New("PARSER_COMMAND is required for sync")
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("parser: %w", err)
	}
	return a.importFile(env("PARSER_MATCHES_FILE", "matches.json"))
}

func (a *archive) downloadMissing(ctx context.Context) error {
	rows, err := a.db.QueryContext(ctx, `SELECT id,source_url FROM demos WHERE file_path IS NULL OR file_path=''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var source string
		if err := rows.Scan(&id, &source); err != nil {
			return err
		}
		if err := a.download(ctx, id, source); err != nil {
			log.Printf("demo %d: %v", id, err)
		}
	}
	return rows.Err()
}

func (a *archive) download(ctx context.Context, id int64, source string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("source returned %s", res.Status)
	}
	path := filepath.Join(a.root, fmt.Sprintf("%d.dem", id))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0640)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(f, res.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	_, err = a.db.Exec(`UPDATE demos SET file_path=?,size_bytes=?,downloaded_at=? WHERE id=?`, path, n, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

func (a *archive) serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/api/demos", a.search)
	mux.HandleFunc("/api/demos/", a.downloadHandler)
	mux.Handle("/", http.FileServer(http.Dir("web")))
	addr := env("LISTEN_ADDR", ":8080")
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (a *archive) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	args := []any{}
	where := []string{"1=1"}
	for _, f := range []string{"map", "player", "team"} {
		if v := strings.TrimSpace(q.Get(f)); v != "" {
			if f == "team" {
				where = append(where, "(m.home_team LIKE ? OR m.away_team LIKE ?)")
				args = append(args, "%"+v+"%", "%"+v+"%")
			} else {
				where = append(where, "d."+f+" LIKE ?")
				args = append(args, "%"+v+"%")
			}
		}
	}
	if v := q.Get("date"); v != "" {
		where = append(where, "m.played_at LIKE ?")
		args = append(args, v+"%")
	}
	limit := 100
	rows, err := a.db.Query(`SELECT d.id,d.match_id,d.player,d.map,d.created_at,m.played_at,m.home_team,m.away_team,d.size_bytes,d.file_path FROM demos d JOIN matches m ON m.id=d.match_id WHERE `+strings.Join(where, " AND ")+` ORDER BY m.played_at DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	type item struct {
		ID          int64  `json:"id"`
		MatchID     int64  `json:"matchId"`
		Player      string `json:"player"`
		Map         string `json:"map"`
		CreatedAt   string `json:"createdAt"`
		PlayedAt    string `json:"playedAt"`
		HomeTeam    string `json:"homeTeam"`
		AwayTeam    string `json:"awayTeam"`
		Size        int64  `json:"size"`
		DownloadURL string `json:"downloadURL"`
	}
	out := []item{}
	for rows.Next() {
		var x item
		if err := rows.Scan(&x.ID, &x.MatchID, &x.Player, &x.Map, &x.CreatedAt, &x.PlayedAt, &x.HomeTeam, &x.AwayTeam, &x.Size, &x.DownloadURL); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		x.DownloadURL = fmt.Sprintf("/api/demos/%d", x.ID)
		out = append(out, x)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (a *archive) downloadHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/demos/")
	var path, name string
	err := a.db.QueryRow(`SELECT file_path,printf('cod2-match-%d.dem',match_id) FROM demos WHERE id=?`, id).Scan(&path, &name)
	if err != nil || path == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, path)
}
