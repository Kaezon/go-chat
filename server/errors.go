package server

import "fmt"

type unknownCommand struct {
	command string
}

func (e unknownCommand) Error() string {
	return fmt.Sprintf("Unknown command %s", e.command)
}

type alreadyRegistered struct {
	info string
}

func (e alreadyRegistered) Error() string {
	return fmt.Sprintf("Tried to add resource to registry, but it was already registered: %s", e.info)
}
