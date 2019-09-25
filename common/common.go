package common

import (
	"bufio"
	"fmt"
	"io"
	"net"
)

const (
	bufferSize = 32*1024
)

type Message struct {
	c *net.Conn
	d *[]byte
}

func StreamToByte(stream io.Reader) (int, *[]byte, error) {
	//TODO: figure out a way to wait for readability or alternatively stream through the tunnel
	buf := make([]byte, bufferSize)
	br, err := stream.Read(buf)
	res := buf[:br]
	return br, &res, err
}

func StreamToChan(stream io.Reader) (int, *[]byte, error) {
	buf := make([]byte, bufferSize)
	reader := bufio.NewReader(stream)
	for br, err := reader.Read(buf); err != io.EOF; {
		fmt.Println(br, err)
	}
	res := buf
	return 0, &res, nil
}