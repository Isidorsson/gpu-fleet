// Command attestref prints the reference values (deploy/attestation/reference.json)
// that accept an untampered bmcsim started with the same seed.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"gpu-fleet/controlplane/internal/bmcsim"
)

const jsonIndent = "  "

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "attestref:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	seed, version, err := parseArgs(args)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(bmcsim.ReferenceValues(seed, version), "", jsonIndent)
	if err != nil {
		return fmt.Errorf("encode reference values: %w", err)
	}
	if _, err := out.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write reference values: %w", err)
	}
	return nil
}

// Both flags are required: a seed that silently differs from the running
// bmcsim makes every GPU fail attestation.
func parseArgs(args []string) (seed uint64, version string, err error) {
	fs := flag.NewFlagSet("attestref", flag.ContinueOnError)
	fs.Uint64Var(&seed, "seed", 0, "bmcsim --seed the reference values are derived from")
	fs.StringVar(&version, "version", "", "reference values version label, e.g. 2026-10-19.1")
	if err := fs.Parse(args); err != nil {
		return 0, "", err
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["seed"] {
		return 0, "", errors.New("--seed is required")
	}
	if version == "" {
		return 0, "", errors.New("--version is required")
	}
	return seed, version, nil
}
