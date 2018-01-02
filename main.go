package main

import "flag"

func main() {
	path := flag.String("-conf", "./config.json", "Config path.")
	(&MainConfig{}).Init(*path)
}
