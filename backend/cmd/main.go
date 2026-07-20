package main

import (
	"log"
	"os"

	"kypost-server/backend/internal/app"
)

func main() {
	if err := app.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
