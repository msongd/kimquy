// Command semctl is a command-line client for semd.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/msongd/kimquy/internal/agent"
	"github.com/msongd/kimquy/internal/client"
	"github.com/msongd/kimquy/internal/config"
	"github.com/msongd/kimquy/internal/events"
	"github.com/msongd/kimquy/internal/kind"
)

const usage = `usage: semctl [-socket path] [-json] <command> [args]

commands:
  search [-k N] [-kind text,office,...] [-root DIR]... <query...>
  preview PATH [LOCATOR]        details and text of an indexed file;
                                LOCATOR as search prints it, e.g. page:3:0-120
  status
  roots                         list indexed directories
  roots set [-exclude GLOB]... DIR...   replace indexed directories
  failures
  pause | resume | reindex
  events                        stream progress events
  agent install [-semd PATH] [-state DIR]   run semd at login (LaunchAgent)
  agent uninstall | agent restart | agent status
`

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "semctl:", err)
		os.Exit(1)
	}
}

func run() error {
	state, err := config.DefaultStateDir()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("semctl", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	socket := fs.String("socket", filepath.Join(state, "semd.sock"), "semd socket")
	asJSON := fs.Bool("json", false, "print raw JSON")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("missing command")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	c := client.NewClient(*socket)
	cmd, args := fs.Arg(0), fs.Args()[1:]

	print := func(v any, human func()) error {
		if *asJSON || human == nil {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		human()
		return nil
	}

	switch cmd {
	case "search":
		sf := flag.NewFlagSet("search", flag.ContinueOnError)
		k := sf.Int("k", 10, "results")
		kinds := sf.String("kind", "", "comma-separated kinds")
		var roots multi
		sf.Var(&roots, "root", "restrict to directory (repeatable)")
		if err := sf.Parse(args); err != nil {
			return err
		}
		req := client.SearchRequest{Q: strings.Join(sf.Args(), " "), K: *k, Roots: roots}
		if *kinds != "" {
			req.Kinds = strings.Split(*kinds, ",")
		}
		for i, r := range req.Roots {
			abs, err := filepath.Abs(r)
			if err != nil {
				return err
			}
			req.Roots[i] = abs
		}
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		res, err := c.Search(tctx, req)
		if err != nil {
			return err
		}
		return print(res, func() {
			for _, h := range res.Hits {
				stale := ""
				if h.Stale {
					stale = " (changed since indexed)"
				}
				fmt.Printf("%.3f  %s  [%s]%s\n", h.Score, h.Path, h.Locator, stale)
				if h.Snippet != "" {
					fmt.Printf("       %s\n", h.Snippet)
				}
			}
			fmt.Fprintf(os.Stderr, "%d hits in %d ms\n", len(res.Hits), res.TookMS)
		})
	case "preview":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: preview PATH [LOCATOR]")
		}
		path, err := filepath.Abs(args[0])
		if err != nil {
			return err
		}
		req := client.PreviewRequest{Path: path}
		if len(args) == 2 {
			if req.Locator, err = kind.ParseLocator(args[1]); err != nil {
				return err
			}
		}
		tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		p, err := c.Preview(tctx, req)
		if err != nil {
			return err
		}
		return print(p, func() { printPreview(p) })
	case "status":
		s, err := c.Status(ctx)
		if err != nil {
			return err
		}
		return print(s, nil)
	case "roots":
		if len(args) > 0 && args[0] == "set" {
			sf := flag.NewFlagSet("roots set", flag.ContinueOnError)
			var ex multi
			sf.Var(&ex, "exclude", "exclude glob (repeatable; replaces the current list when given)")
			if err := sf.Parse(args[1:]); err != nil {
				return err
			}
			req := client.RootsRequest{Roots: []string{}}
			if len(ex) > 0 {
				req.Excludes = ex
			}
			for _, r := range sf.Args() {
				abs, err := filepath.Abs(r)
				if err != nil {
					return err
				}
				req.Roots = append(req.Roots, abs)
			}
			r, err := c.SetRoots(ctx, req)
			if err != nil {
				return err
			}
			return print(r, nil)
		}
		if len(args) > 0 {
			return fmt.Errorf("roots: unknown subcommand %q", args[0])
		}
		r, err := c.Roots(ctx)
		if err != nil {
			return err
		}
		return print(r, nil)
	case "failures":
		f, err := c.Failures(ctx)
		if err != nil {
			return err
		}
		return print(f, func() {
			for _, x := range f {
				fmt.Printf("%s\n    %s\n", x.Path, x.Error)
			}
			fmt.Fprintf(os.Stderr, "%d failures\n", len(f))
		})
	case "pause":
		return c.Pause(ctx)
	case "resume":
		return c.Resume(ctx)
	case "reindex":
		return c.Reindex(ctx)
	case "agent":
		return agentCmd(args, state, print)
	case "events":
		err := c.Events(ctx, func(ev events.Event) {
			if ev.Type == "open" {
				return // made by the client, not semd
			}
			if *asJSON {
				b, _ := json.Marshal(ev)
				fmt.Println(string(b))
				return
			}
			fmt.Printf("%s %-8s %s %s\n", ev.Time.Format("15:04:05"), ev.Type, ev.Path, ev.Msg)
		})
		if errors.Is(err, context.Canceled) {
			return nil
		}
		return err
	}
	fs.Usage()
	return fmt.Errorf("unknown command %q", cmd)
}

// printPreview prints a preview's details, then its text with the match
// between » and «.
func printPreview(p client.Preview) {
	fmt.Printf("%s\n  %s, %d bytes, %s, %d vectors\n", p.Path, p.Kind, p.Size, p.Status, p.Vectors)
	if !p.Modified.IsZero() {
		fmt.Printf("  modified %s\n", p.Modified.Format(time.DateTime))
	}
	if !p.Indexed.IsZero() {
		fmt.Printf("  indexed  %s\n", p.Indexed.Format(time.DateTime))
	}
	if p.Stale {
		fmt.Println("  changed since indexed")
	}
	switch {
	case p.Pages > 0:
		fmt.Printf("  %d pages\n", p.Pages)
	case p.Duration > 0:
		fmt.Printf("  %d ms\n", p.Duration)
	case p.Width > 0:
		fmt.Printf("  %d×%d pixels\n", p.Width, p.Height)
	}
	if p.TextError != "" {
		fmt.Println("  no text:", p.TextError)
	}
	if p.Text == "" {
		return
	}
	fmt.Printf("--- bytes %d–%d of %d\n", p.TextStart, p.TextStart+int64(len(p.Text)), p.TextTotal)
	if p.MatchEnd > p.MatchStart {
		fmt.Printf("%s»%s«%s\n", p.Text[:p.MatchStart], p.Text[p.MatchStart:p.MatchEnd], p.Text[p.MatchEnd:])
	} else {
		fmt.Println(p.Text)
	}
}

// agentCmd manages the LaunchAgent that runs semd.
func agentCmd(args []string, defState string, print func(any, func()) error) error {
	if len(args) == 0 {
		return errors.New("agent: missing subcommand (install, uninstall, restart, status)")
	}
	af := flag.NewFlagSet("agent "+args[0], flag.ContinueOnError)
	label := af.String("label", agent.DefaultLabel, "launchd label")
	semd := af.String("semd", "", "semd executable (default: next to semctl)")
	state := af.String("state", defState, "semd state directory")
	if err := af.Parse(args[1:]); err != nil {
		return err
	}
	if af.NArg() != 0 {
		return fmt.Errorf("agent: unexpected arguments %v", af.Args())
	}
	if *semd == "" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if exe, err = filepath.EvalSymlinks(exe); err != nil {
			return err
		}
		*semd = filepath.Join(filepath.Dir(exe), "semd")
	}
	m, err := agent.Default()
	if err != nil {
		return err
	}
	switch args[0] {
	case "uninstall":
		return m.Uninstall(*label)
	case "restart":
		return m.Restart(*label)
	case "install", "status":
		spec, err := agent.Semd(*label, *semd, *state, agent.BundleID)
		if err != nil {
			return err
		}
		if args[0] == "install" {
			if err := m.Install(spec); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "installed %s; log: %s\n", m.PlistPath(*label), spec.LogPath)
		}
		st, err := m.Status(spec)
		if err != nil {
			return err
		}
		return print(st, nil)
	}
	return fmt.Errorf("agent: unknown subcommand %q", args[0])
}
