// Command invarios is the appliance's CLI entrypoint: run with no subcommand
// it starts the appliance's PID 1 supervisor loop, and "invarios build"
// assembles the bootable appliance image.
package main

import "github.com/invarios/invarios/cmd"

func main() {
	cmd.Execute()
}
