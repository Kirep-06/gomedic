package main

import (
	"fmt"
	"os"

	"github.com/Kirep-06/gomedic/internal/version"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version" || os.Args[1] == "-v") {
		fmt.Println(version.String())
		return
	}
	fmt.Println("gomedic - zero-dependency CLI diagnostics for Go services")
	fmt.Println("Usage: gomedic [version]")
}
