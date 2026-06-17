// Command bookwatch.
//
//	bookwatch serve                      run the HTTP server + scheduler
//	bookwatch [check] [-root] [-quiet] [-record] [-write]
//	bookwatch add URL [-vault] [-dir] [-attach]
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"

	"bookwatch/internal/config"
	"bookwatch/internal/notes"
	"bookwatch/internal/scheduler"
	"bookwatch/internal/scraper"
	"bookwatch/internal/server"
	"bookwatch/internal/service"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		runServe(os.Args[2:])
	case "add":
		runAdd(os.Args[2:])
	case "check":
		runCheck(os.Args[2:])
	default:
		runCheck(os.Args[1:])
	}
}

// ── serve ─────────────────────────────────────────────────────
func runServe(argv []string) {
	cfg := config.Default()
	if cfg.Password == "" {
		log.Fatal("BOOKWATCH_PASSWORD is required to run the server (write endpoints need it)")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Fatal("db open: ", err)
	}
	defer st.Close()

	sc := scraper.New(cfg.UserAgent, cfg.Timeout)

	sched := scheduler.New(func(progress func(i, total int, title string)) (service.CheckSummary, error) {
		scanRoot := cfg.ScanRoot
		if v, ok, _ := st.GetSetting("scan_root"); ok && v != "" {
			scanRoot = v
		}
		// Detect-only: a check never writes the vault. Writes happen on an
		// explicit apply (POST /api/apply), never on a cron/manual check.
		return service.RunCheck(sc, st, scanRoot, false, progress)
	})
	if err := sched.Start(cfg.CheckCron); err != nil {
		log.Fatal("scheduler: ", err)
	}
	defer sched.Stop()

	srv := server.New(cfg, st, sc, sched)
	addr := ":" + cfg.Port
	log.Printf("BookWatch listening on http://localhost%s (cron %q, scan %s)", addr, cfg.CheckCron, cfg.ScanRoot)
	if err := http.ListenAndServe(addr, srv.Handler()); err != nil {
		log.Fatal(err)
	}
}

// ── check ─────────────────────────────────────────────────────
func runCheck(argv []string) {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	root := fs.String("root", `./vault/LightNovel`,
		"vault folder to scan for #LightNovel notes")
	quiet := fs.Bool("quiet", false, "suppress per-novel progress")
	record := fs.Bool("record", false, "persist run + updates to the SQLite DB")
	write := fs.Bool("write", false, "APPLY updates to the vault (bump Volumes + Last Update)")
	fs.Parse(argv)

	cfg := config.Default()
	sc := scraper.New(cfg.UserAgent, cfg.Timeout)

	var st *store.Store
	if *record {
		var err error
		if st, err = store.Open(cfg.DBPath); err != nil {
			fmt.Fprintln(os.Stderr, "db open error:", err)
			os.Exit(1)
		}
		defer st.Close()
	}

	progress := func(i, total int, title string) {
		if !*quiet {
			fmt.Printf("[%d/%d] %s\n", i, total, title)
		}
	}

	sum, err := service.RunCheck(sc, st, *root, *write, progress)
	if err != nil {
		fmt.Fprintln(os.Stderr, "check error:", err)
		os.Exit(1)
	}

	mode := "read-only"
	if *write {
		mode = "WRITE — vault updated"
	}
	fmt.Printf("\n── Updates (%s) ──\n", mode)
	for _, u := range sum.Updates {
		flag := ""
		if u.Wrote {
			flag = " ✓"
		}
		fmt.Printf("  [NEW]%s %s: %d -> %d  %s\n", flag, u.Title, u.OldVolumes, u.NewVolumes, u.Link)
	}
	if st != nil {
		fmt.Printf("\nRecorded run to %s\n", cfg.DBPath)
	}
	fmt.Printf("\nDone. %d notes, %d with new volumes, %d errors.\n", sum.Checked, sum.Updated, sum.Errors)
}

// ── add ───────────────────────────────────────────────────────
func runAdd(argv []string) {
	cfg := config.Default()

	fs := flag.NewFlagSet("add", flag.ExitOnError)
	vaultDir := fs.String("vault", cfg.VaultDir, "vault root")
	dir := fs.String("dir", cfg.NewNoteDir, "new-note folder (relative to vault)")
	attach := fs.String("attach", cfg.AttachmentsDir, "attachments folder (relative to vault)")
	fs.Parse(argv)

	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bookwatch add <url> [-vault DIR] [-dir REL] [-attach REL]")
		os.Exit(2)
	}
	url := fs.Arg(0)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db open error:", err)
		os.Exit(1)
	}
	defer st.Close()

	sc := scraper.New(cfg.UserAgent, cfg.Timeout)
	opts := notes.Options{VaultDir: *vaultDir, NewNoteDir: *dir, AttachmentsDir: *attach}

	fmt.Printf("Adding %s\n", url)
	rl := sources.NewResolver(st).For(url)
	res, err := notes.Create(opts, sc, st, rl, url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "add error:", err)
		os.Exit(1)
	}
	if _, err := st.UpsertBook(res.Title, url, res.Path, res.Volumes); err != nil {
		fmt.Fprintln(os.Stderr, "upsert book error:", err)
	}
	fmt.Printf("Created: %s (%d volumes)\n  %s\n", res.Title, res.Volumes, res.Path)
}
