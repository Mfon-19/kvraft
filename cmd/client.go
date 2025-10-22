package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	conn, err := net.Dial("tcp", "localhost:8080")
	if err != nil {
		fmt.Println("error while connecting to server: ", err)
		return
	}
	defer conn.Close()

	fmt.Println("connected to server at:", conn.RemoteAddr())
	fmt.Println("Enter commands (e.g., 'open mydb', 'put handle key value', 'get handle key'):")

	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("> ")
		scanner.Scan()
		input := scanner.Text()

		if input == "" {
			continue
		}

		// Send command
		_, err := conn.Write([]byte(input))
		if err != nil {
			fmt.Println("error sending message to server:", err)
			return
		}

		response := make([]byte, 1024)
		n, err := conn.Read(response)
		if err != nil {
			fmt.Println("error reading response:", err)
			return
		}
		fmt.Println("Response:", string(response[:n]))

		if input == "stop" {
			return
		}
	}
}
