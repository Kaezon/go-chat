package server

import "fmt"

// CommandProcessor handles chat commands and their args
type CommandProcessor func(server Server, command []string, source client) (ok bool, err error)

func nickHandler(server Server, command []string, source client) (ok bool, err error) {
	oldNick := source.nickname
	source.nickname = command[1]
	server.Broadcast(fmt.Sprintf("Server: %s is now known as %s", oldNick, source.nickname))
	return true, nil
}
