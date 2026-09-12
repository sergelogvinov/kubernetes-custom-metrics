package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := NewRootCommand()
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	cmd.SetArgs(args)

	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, err)

		if _, ok := errors.AsType[*usageError](err); ok {
			return 2
		}

		return 1
	}

	return 0
}
