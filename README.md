# go-chat

## A basic TCP chat server in Go

This is a basic chat server implementation written in Go. It's not exactly minimalist,
but it is a good starting point for something more feature filled.

The server listens on a TCP port and runs all client connections in goroutines.

In the future, a basic client implementation will be added.

## Usage

```go
import "bytes"
import "fmt"
import "log"
import "github.com/kaezon/go-chat/server"

logBuf := bytes.Buffer
logger := log.New(&buf, "logger: ", log.Lshortfile)

server := server.New(&logger)

fmt.Println("Starting server...")
err := server.Start(":8081")
if err != nil {
	fmt.Println("[ERROR] ", err)
	return
}

time.Sleep(10 * time.Second)

fmt.Println("Stopping server...")
server.Shutdown(60)

fmt.Println("--Done--")
```
