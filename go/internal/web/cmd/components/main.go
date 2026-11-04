// Command components writes assets/components.js: the imports for every Web Awesome component
// the templates use. Run it after adding a <wa-...> tag (the vendor tool runs it too), from go/:
//
//	go run ./internal/web/cmd/components
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/rnkch/flimmerkiste/internal/web/components"
)

func main() {
	webDir, _ := filepath.Abs("internal/web")
	if _, err := os.Stat(filepath.Join(webDir, "assets", "app.js")); err != nil {
		log.Fatal("run from go/")
	}
	names, err := components.Generate(webDir)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d components: %v\n", len(names), names)
}
