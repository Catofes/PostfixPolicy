package main

import (
	"flag"
	"context"
	"github.com/Catofes/PostfixPolicy/policy"
)

func main() {
	path := flag.String("conf", "./config.json", "Config path.")
	flag.Parse()
	(&policy.MainPolicy{MainConfig: *(&policy.MainConfig{}).Init(*path)}).Init(context.Background()).Run()
}
