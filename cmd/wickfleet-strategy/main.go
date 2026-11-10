// wickfleet-strategy checks a strategy spec, prints its plain-language
// readback, generates the freqtrade strategy, or drafts one from a sentence.
//
//	wickfleet-strategy check    spec.json
//	wickfleet-strategy describe spec.json
//	wickfleet-strategy gen      spec.json > Name.py
//	wickfleet-strategy draft -ollama http://host:11434 -model qwen3:8b "sentence"
//	wickfleet-strategy eval  -ollama http://host:11434 -model a,b cases.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"wickfleet/internal/drafter"
	"wickfleet/internal/strategy"
)

const usage = "usage: wickfleet-strategy check|describe|gen <spec.json> | draft|eval -ollama URL -model M <sentence|cases.json>"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, out, errw io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	switch args[0] {
	case "draft", "eval":
		return runModel(args[0], args[1:], out, errw)
	}
	if len(args) != 2 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	raw, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	s, err := strategy.Parse(raw)
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	switch args[0] {
	case "check":
		fmt.Fprintf(out, "%s: ok\n", s.Name)
	case "describe":
		fmt.Fprintln(out, s.Describe())
	case "gen":
		src, err := s.Python()
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		fmt.Fprint(out, src)
	default:
		fmt.Fprintln(errw, usage)
		return 2
	}
	return 0
}

func runModel(cmd string, args []string, out, errw io.Writer) int {
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(errw)
	base := fs.String("ollama", "http://127.0.0.1:11434", "Ollama base URL")
	models := fs.String("model", "", "model tag; for eval a comma list")
	if err := fs.Parse(args); err != nil || *models == "" || fs.NArg() != 1 {
		fmt.Fprintln(errw, usage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	if cmd == "draft" {
		r, err := drafter.Drafter{Providers: []drafter.Provider{drafter.Ollama{Base: *base, Model: *models}}}.Draft(ctx, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(errw, err)
			return 1
		}
		spec, _ := json.MarshalIndent(r.Spec, "", "  ")
		fmt.Fprintf(out, "%s\n\n%s\n\n(%s, %d attempt(s), %.1fs)\n", spec, r.Describe, r.Provider, r.Attempts, r.Seconds)
		return 0
	}
	cases, err := drafter.LoadCases(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(errw, err)
		return 1
	}
	var scores []drafter.Score
	for _, m := range strings.Split(*models, ",") {
		fmt.Fprintf(out, "%s\n", m)
		scores = append(scores, drafter.Eval(ctx, drafter.Ollama{Base: *base, Model: m}, cases, out))
	}
	fmt.Fprintf(out, "\n%-28s %6s %8s %8s\n", "model", "valid", "correct", "median")
	for _, s := range scores {
		fmt.Fprintf(out, "%-28s %3d/%-2d %5d/%-2d %7.1fs\n", s.Provider, s.Valid, s.Cases, s.Correct, s.Cases, s.Median.Seconds())
	}
	return 0
}
