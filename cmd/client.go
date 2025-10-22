package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
)

func main() {
	conn, err := net.Dial("localhost", "8080")
	if err != nil {
		fmt.Println("error while connecting to server: ", err)
		return
	}

	fmt.Println("connected to server at: ", conn.RemoteAddr())

	scanner := bufio.NewScanner(os.Stdin)
	for {
		scanner.Scan()
		input := scanner.Text()

		_, err := conn.Write([]byte(input))
		if err != nil {
			fmt.Println("error sending message to server: ", err)
		}
	}
}
