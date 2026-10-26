package main

import (
	"context"
	"os"
	"os/signal"

	appkit "github.com/soundadam/appkit/go"
	appcli "github.com/soundadam/appkit/go/cli"

	"github.com/soundadam/chatvault/internal/cli"
)

// version is the release version; release builds set it with
// -ldflags "-X main.version=<ver>" from the tag.
var version = "dev"

func main() {
	identity := appkit.App{Name: "chatvault", Version: version, Repo: "soundadam/chatvault"}
	app := &cli.App{
		In:        os.Stdin,
		Out:       os.Stdout,
		Err:       os.Stderr,
		StdinTTY:  isTerminal(os.Stdin),
		StdoutTTY: isTerminal(os.Stdout),
		StderrTTY: isTerminal(os.Stderr),
		Identity:  identity,
	}
	notice := appcli.StartNotice(identity)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := app.Execute(ctx, os.Args[1:])
	stop()
	notice.Print(os.Stderr)
	os.Exit(code)
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
