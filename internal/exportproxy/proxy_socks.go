package exportproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
)

func serveSOCKS(client net.Conn, config Config, dialer *net.Dialer, resolver *net.Resolver) error {
	return serveSOCKSContext(context.Background(), client, config, dialer, resolver)
}

func serveSOCKSContext(ctx context.Context, client net.Conn, config Config, dialer *net.Dialer, resolver *net.Resolver) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stopClient := context.AfterFunc(ctx, func() { _ = client.Close() })
	defer stopClient()
	reader := bufio.NewReader(client)
	version, err := reader.ReadByte()
	if err != nil || version != 5 {
		return errors.New("unsupported SOCKS version")
	}
	methodCount, err := reader.ReadByte()
	if err != nil {
		return err
	}
	methods := make([]byte, methodCount)
	if _, err := io.ReadFull(reader, methods); err != nil {
		return err
	}
	chosen := byte(0xff)
	if config.AuthEnabled && hasMethod(methods, 2) {
		chosen = 2
	} else if !config.AuthEnabled && hasMethod(methods, 0) {
		chosen = 0
	}
	if _, err := client.Write([]byte{5, chosen}); err != nil || chosen == 0xff {
		return errors.New("no acceptable SOCKS authentication method")
	}
	if chosen == 2 {
		if err := socksAuthenticate(reader, client, config); err != nil {
			return err
		}
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return err
	}
	if header[0] != 5 || header[1] != 1 {
		_ = writeSocksReply(client, 7)
		return errors.New("only SOCKS5 CONNECT is supported")
	}
	host, port, err := readSocksAddress(reader, header[3])
	if err != nil {
		_ = writeSocksReply(client, 1)
		return err
	}
	dialContext, cancel := context.WithTimeout(ctx, proxyTimeout)
	target, err := dialTarget(dialContext, net.JoinHostPort(host, strconv.Itoa(port)), dialer, resolver)
	cancel()
	if err != nil {
		_ = writeSocksReply(client, 5)
		return err
	}
	defer target.Close()
	stopClosing := context.AfterFunc(ctx, func() { _ = target.Close() })
	defer stopClosing()
	if err := writeSocksReply(client, 0); err != nil {
		return err
	}
	if buffered := reader.Buffered(); buffered > 0 {
		if value, err := reader.Peek(buffered); err == nil {
			_, _ = target.Write(value)
			_, _ = reader.Discard(buffered)
		}
	}
	pipe(client, target)
	return nil
}

func socksAuthenticate(reader *bufio.Reader, writer io.Writer, config Config) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil || header[0] != 1 {
		return errors.New("invalid SOCKS authentication request")
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(reader, username); err != nil {
		return err
	}
	length, err := reader.ReadByte()
	if err != nil {
		return err
	}
	password := make([]byte, int(length))
	if _, err := io.ReadFull(reader, password); err != nil {
		return err
	}
	if string(username) != config.Username || string(password) != config.Password {
		_, _ = writer.Write([]byte{1, 1})
		return errors.New("SOCKS authentication failed")
	}
	_, err = writer.Write([]byte{1, 0})
	return err
}

func hasMethod(methods []byte, wanted byte) bool {
	for _, method := range methods {
		if method == wanted {
			return true
		}
	}
	return false
}

func readSocksAddress(reader *bufio.Reader, kind byte) (string, int, error) {
	var host string
	switch kind {
	case 1:
		value := make([]byte, 4)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", 0, err
		}
		host = net.IP(value).String()
	case 3:
		length, err := reader.ReadByte()
		if err != nil {
			return "", 0, err
		}
		value := make([]byte, int(length))
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", 0, err
		}
		host = string(value)
	case 4:
		value := make([]byte, 16)
		if _, err := io.ReadFull(reader, value); err != nil {
			return "", 0, err
		}
		host = net.IP(value).String()
	default:
		return "", 0, fmt.Errorf("unsupported SOCKS address type %d", kind)
	}
	value := make([]byte, 2)
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", 0, err
	}
	return host, int(binary.BigEndian.Uint16(value)), nil
}

func writeSocksReply(connection net.Conn, code byte) error {
	_, err := connection.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
