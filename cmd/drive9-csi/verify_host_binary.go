package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

func runVerifyHostBinaryCommand(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("verify-host-binary", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var path string
	var targetArch string
	flags.StringVar(&path, "path", "", "static Linux ELF path")
	flags.StringVar(&targetArch, "target-arch", "", "target architecture")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("verify-host-binary: unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	if path == "" || targetArch == "" {
		return fmt.Errorf("verify-host-binary: --path and --target-arch are required")
	}
	_, digest, err := readValidatedELF(path, targetArch)
	if err != nil {
		return fmt.Errorf("verify-host-binary: %w", err)
	}
	if stdout != nil {
		_, err = fmt.Fprintf(stdout, "sha256=%s\n", digest)
	}
	return err
}
