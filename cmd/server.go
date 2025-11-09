package main

import (
	"fmt"
	"kvraft/kvstore"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	listener, err := net.Listen("tcp", ":8080")
	if err != nil {
		fmt.Println("error starting server: ", err)
		return
	}

	fmt.Println("server listening on :8080")

	conn, err := listener.Accept()
	if err != nil {
		fmt.Println("error accepting connection: ", err)
		return
	}

	fmt.Println("client connected on: ", conn.RemoteAddr())

	connections := make(map[string]*kvstore.DB)
	l := log.New(os.Stdout, "[SERVER] - ", log.Ltime)
	// this assumes no errors in input
	for {
		message := make([]byte, 1024)
		n, err := conn.Read(message)
		if err != nil {
			_, err := conn.Write([]byte("error reading message from client"))
			if err != nil {
				fmt.Println("failed to send response to client: ", err)
			}
		}
		if n == 0 {
			continue
		}

		l.Printf("received command from client: %s\n", string(message))

		parts := strings.Fields(string(message[:n]))
		command := parts[0]
		args := parts[1:]

		switch command {
		case "open":
			dirName := args[0]
			idx := len(connections) + 1

			// the handle is just a concatenation of directory name and
			// the number of connections the server is currently handling
			dbHandle := dirName + strconv.Itoa(idx)

			// open a database
			db, err := kvstore.Open(dirName)
			if err != nil {
				response := "failed to open database: " + err.Error()
				_, err := conn.Write([]byte(response))
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			} else {
				// now this dbHandle points to the database object
				// whose methods will be called when the client uses the handle
				connections[dbHandle] = db
				_, err = conn.Write([]byte(dbHandle))
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			}

		case "put":
			dbHandle := args[0]
			key := args[1]
			value := args[2]

			err := connections[dbHandle].Put(key, value)
			if err != nil {
				response := "failed to put value: " + err.Error()
				_, err := conn.Write([]byte(response))
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			} else {
				response := "value put successfully"
				_, err := conn.Write([]byte(response))
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			}

		case "get":
			dbHandle := args[0]
			key := args[1]

			value, err := connections[dbHandle].Get(key)
			if err != nil {
				response := "failed to get value: " + err.Error()
				_, err := conn.Write([]byte(response))
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			} else {
				_, err := conn.Write(value)
				if err != nil {
					fmt.Println("failed to send response to client: ", err)
				}
			}

		case "stop":
			err := conn.Close()
			if err != nil {
				return
			}
			return
		}
	}
}
