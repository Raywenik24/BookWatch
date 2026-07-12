// Command bookwatch.
//
//	bookwatch serve                      run the HTTP server + scheduler
//	bookwatch [check] [-root] [-quiet] [-record] [-write]
//	bookwatch add URL [-vault] [-dir] [-attach]
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"bookwatch/internal/config"
	"bookwatch/internal/notes"
	"bookwatch/internal/provider"
	"bookwatch/internal/scheduler"
	"bookwatch/internal/scraper"
	"bookwatch/internal/server"
	"bookwatch/internal/service"
	"bookwatch/internal/sources"
	"bookwatch/internal/store"
)

func main() {
	if portable {
		runPortable()
		return
	}

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

// ── portable ──────────────────────────────────────────────────
// runPortable is the bookwatch-portable.exe entrypoint (#78): double-click
// starts the server, generating + persisting a password on first run, and
// keeps the console open on exit/error so the window doesn't just vanish.
func runPortable() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("panic:", r)
		}
		fmt.Println("\nPress Enter to close…")
		bufio.NewReader(os.Stdin).ReadString('\n')
	}()

	// Resolve .env/db/vault relative to the exe's own folder, not whatever
	// directory Explorer happened to launch it from.
	if exe, err := os.Executable(); err == nil {
		if err := os.Chdir(filepath.Dir(exe)); err != nil {
			fmt.Println("warning: could not switch to exe dir:", err)
		}
	}

	ensurePortablePassword()

	if err := runServeErr(); err != nil {
		fmt.Println("error:", err)
	}
}

// ensurePortablePassword generates + persists a random password into a
// gitignored .env next to the exe if none is configured yet, so a
// double-click has no manual setup step.
func ensurePortablePassword() {
	if config.Default().Password != "" {
		return
	}

	buf := make([]byte, 18)
	if _, err := rand.Read(buf); err != nil {
		fmt.Println("error generating password:", err)
		return
	}
	pw := base64.RawURLEncoding.EncodeToString(buf)

	f, err := os.OpenFile(".env", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Println("warning: could not write .env:", err)
	} else {
		fmt.Fprintf(f, "BOOKWATCH_PASSWORD=%s\n", pw)
		f.Close()
	}
	os.Setenv("BOOKWATCH_PASSWORD", pw)
	fmt.Println("No password configured — generated one and saved it to .env:")
	fmt.Println("  " + pw)
	fmt.Println("Edit .env next to this exe to change it.")
}

// ── serve ─────────────────────────────────────────────────────
func runServe(argv []string) {
	if err := runServeErr(); err != nil {
		log.Fatal(err)
	}
}

// runServeErr does the actual serving and returns errors instead of calling
// log.Fatal, so the portable entrypoint (which needs its deferred console
// pause to run on failure) can report the error itself instead of the
// process exiting out from under it.
func runServeErr() error {
	cfg := config.Default()
	if cfg.Password == "" {
		return fmt.Errorf("BOOKWATCH_PASSWORD is required to run the server (write endpoints need it)")
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	defer st.Close()

	sc := scraper.New(cfg.UserAgent, cfg.Timeout)
	ol := provider.NewOpenLibrary(cfg.UserAgent, cfg.Timeout)
	gb := provider.NewGoogleBooks(cfg.GBKey, cfg.Timeout)
	gr := provider.NewGoodreads(cfg.UserAgent, cfg.Timeout)
	lc := provider.NewLubimyczytac(cfg.UserAgent, cfg.Timeout)

	// Detect-only: a check never writes the vault. Writes happen on an
	// explicit apply (POST /api/apply), never on a cron/manual check.
	sched := scheduler.New(
		func(progress func(i, total int, title string)) (service.CheckSummary, error) {
			return service.RunCheck(sc, st, ol, lc, server.ScanRoots(cfg, st), false, progress)
		},
		func(progress func(i, total int, title string)) (service.CheckSummary, error) {
			return service.RunLNCheck(sc, st, server.ScanRoots(cfg, st), false, progress)
		},
		func(progress func(i, total int, title string)) (service.CheckSummary, error) {
			return service.RunTrackerPoll(st, ol, lc, progress)
		},
	)
	lnCron, trackerCron := cfg.CheckCron, cfg.TrackerCron
	if v, ok, _ := st.GetSetting("ln_check_cron"); ok && v != "" {
		lnCron = v
	}
	if v, ok, _ := st.GetSetting("tracker_check_cron"); ok && v != "" {
		trackerCron = v
	}
	if err := sched.Start(lnCron, trackerCron); err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	defer sched.Stop()

	srv := server.New(cfg, st, sc, sched, ol, gb, gr, lc)
	addr := ":" + cfg.Port
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	// Listen on a goroutine so the main path can wait for a shutdown signal and
	// drain cleanly — letting the deferred sched.Stop()/st.Close() actually run
	// and any in-flight vault write finish.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Printf("BookWatch listening on http://localhost%s (LN cron %q, tracker cron %q, scan %s)", addr, lnCron, trackerCron, cfg.ScanRoot)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	stop() // restore default signal handling so a second Ctrl+C force-quits
	log.Println("shutting down…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
	return nil
}

// ── check ─────────────────────────────────────────────────────
func runCheck(argv []string) {
	cfg := config.Default()

	fs := flag.NewFlagSet("check", flag.ExitOnError)
	root := fs.String("root", cfg.ScanRoot, "vault folder to scan for #LightNovel notes")
	quiet := fs.Bool("quiet", false, "suppress per-novel progress")
	record := fs.Bool("record", false, "persist run + updates to the SQLite DB")
	write := fs.Bool("write", false, "APPLY updates to the vault (bump Volumes + Last Update)")
	fs.Parse(argv)
	sc := scraper.New(cfg.UserAgent, cfg.Timeout)
	ol := provider.NewOpenLibrary(cfg.UserAgent, cfg.Timeout)
	lc := provider.NewLubimyczytac(cfg.UserAgent, cfg.Timeout)

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

	sum, err := service.RunCheck(sc, st, ol, lc, []string{*root}, *write, progress)
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
	if sum.TrackersChecked > 0 || sum.TrackingErrors > 0 {
		fmt.Printf("%d authors polled, %d new releases, %d tracking errors.\n", sum.TrackersChecked, sum.NewReleases, sum.TrackingErrors)
	}
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
	res, err := notes.Create(opts, sc, st, rl, url, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "add error:", err)
		os.Exit(1)
	}
	if _, err := st.UpsertBook(res.Title, url, res.Path, res.Volumes, res.Cover, "", nil, "ln", ""); err != nil {
		fmt.Fprintln(os.Stderr, "upsert book error:", err)
	}
	fmt.Printf("Created: %s (%d volumes)\n  %s\n", res.Title, res.Volumes, res.Path)
}
