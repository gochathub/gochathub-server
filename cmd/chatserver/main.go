// Command chat-server is the single deployable binary: API + WebSocket admin CLI in one.
package main

import "github.com/gochathub/gochathub-server/internal/cli"

func main() {
	cli.Execute()
}
