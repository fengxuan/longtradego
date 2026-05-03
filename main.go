package main

import (
	"log"
	"os"

	"longtradego/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
