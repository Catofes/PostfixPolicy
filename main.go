package main

import (
	"context"
	"flag"
	"github.com/Catofes/PostfixPolicy/policy"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	path := flag.String("conf", "./config.json", "Config path.")
	flag.Parse()
	config, err := (&policy.MainConfig{}).Init(*path)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app, err := (&policy.MainPolicy{MainConfig: *config}).Init(ctx)
	if err != nil {
		log.Fatal(err)
	}
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}

}
