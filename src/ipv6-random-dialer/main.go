package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	socksVersion5           = 0x05
	socksNoAuth             = 0x00
	socksNoAcceptable       = 0xff
	socksConnect            = 0x01
	socksSucceeded          = 0x00
	socksGeneralFailure     = 0x01
	socksNotAllowed         = 0x02
	socksHostUnreachable    = 0x04
	socksCommandUnsupported = 0x07
	socksAddressUnsupported = 0x08
	socksIPv4               = 0x01
	socksDomain             = 0x03
	socksIPv6               = 0x04
)

type dialServer struct {
	prefix       netip.Prefix
	resolver     *net.Resolver
	dialTimeout  time.Duration
	totalTimeout time.Duration
}

func main() {
	listenAddress := flag.String("listen", "127.0.0.1:27324", "local SOCKS5 listen address")
	prefixText := flag.String("prefix", "", "routed IPv6 /64 used for source addresses (required)")
	flag.Parse()

	if *prefixText == "" {
		log.Fatal("-prefix is required")
	}
	prefix, err := netip.ParsePrefix(*prefixText)
	if err != nil {
		log.Fatalf("invalid IPv6 prefix: %v", err)
	}
	prefix = prefix.Masked()
	if !prefix.Addr().Is6() || prefix.Bits() != 64 {
		log.Fatal("source prefix must be an IPv6 /64")
	}

	listener, err := net.Listen("tcp4", *listenAddress)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	server := &dialServer{
		prefix:       prefix,
		resolver:     net.DefaultResolver,
		dialTimeout:  15 * time.Second,
		totalTimeout: 35 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	log.Printf("random IPv6 dialer listening on %s with prefix %s", *listenAddress, prefix)
	for {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("accept: %v", acceptErr)
			continue
		}
		go server.handleConnection(connection)
	}
}

func (server *dialServer) handleConnection(client net.Conn) {
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(10 * time.Second))

	if err := negotiateNoAuthentication(client); err != nil {
		log.Printf("SOCKS greeting from %s: %v", client.RemoteAddr(), err)
		return
	}

	host, port, err := readConnectRequest(client)
	if err != nil {
		replyCode := byte(socksGeneralFailure)
		if errors.Is(err, errCommandUnsupported) {
			replyCode = socksCommandUnsupported
		} else if errors.Is(err, errAddressUnsupported) {
			replyCode = socksAddressUnsupported
		}
		_ = writeReply(client, replyCode, nil)
		log.Printf("SOCKS request from %s: %v", client.RemoteAddr(), err)
		return
	}
	_ = client.SetDeadline(time.Time{})

	ctx, cancel := context.WithTimeout(context.Background(), server.totalTimeout)
	defer cancel()

	upstream, err := server.dialTarget(ctx, host, port)
	if err != nil {
		replyCode := byte(socksHostUnreachable)
		if errors.Is(err, errDestinationNotAllowed) {
			replyCode = socksNotAllowed
		}
		_ = writeReply(client, replyCode, nil)
		log.Printf("dial %s:%d: %v", host, port, err)
		return
	}
	defer upstream.Close()

	if err := writeReply(client, socksSucceeded, upstream.LocalAddr()); err != nil {
		return
	}
	relay(client, upstream)
}

func negotiateNoAuthentication(connection net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(connection, header); err != nil {
		return err
	}
	if header[0] != socksVersion5 || header[1] == 0 {
		return errors.New("invalid SOCKS5 greeting")
	}

	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(connection, methods); err != nil {
		return err
	}
	for _, method := range methods {
		if method == socksNoAuth {
			_, err := connection.Write([]byte{socksVersion5, socksNoAuth})
			return err
		}
	}
	_, _ = connection.Write([]byte{socksVersion5, socksNoAcceptable})
	return errors.New("no supported authentication method")
}

var (
	errCommandUnsupported    = errors.New("only CONNECT is supported")
	errAddressUnsupported    = errors.New("address type is unsupported")
	errDestinationNotAllowed = errors.New("destination is not public")
)

func readConnectRequest(connection net.Conn) (string, uint16, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(connection, header); err != nil {
		return "", 0, err
	}
	if header[0] != socksVersion5 {
		return "", 0, errors.New("invalid SOCKS5 request version")
	}
	if header[1] != socksConnect {
		return "", 0, errCommandUnsupported
	}

	var host string
	switch header[3] {
	case socksIPv4:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(connection, address); err != nil {
			return "", 0, err
		}
		host = net.IP(address).String()
	case socksIPv6:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(connection, address); err != nil {
			return "", 0, err
		}
		host = net.IP(address).String()
	case socksDomain:
		length := make([]byte, 1)
		if _, err := io.ReadFull(connection, length); err != nil {
			return "", 0, err
		}
		if length[0] == 0 {
			return "", 0, errors.New("empty domain name")
		}
		domain := make([]byte, int(length[0]))
		if _, err := io.ReadFull(connection, domain); err != nil {
			return "", 0, err
		}
		host = string(domain)
	default:
		return "", 0, errAddressUnsupported
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(connection, portBytes); err != nil {
		return "", 0, err
	}
	return host, binary.BigEndian.Uint16(portBytes), nil
}

func (server *dialServer) dialTarget(ctx context.Context, host string, port uint16) (net.Conn, error) {
	ipv6Targets, ipv4Targets, err := server.resolvePublicTargets(ctx, host)
	if err != nil {
		return nil, err
	}

	var lastError error
	if len(ipv6Targets) > 0 {
		sourceAddress, randomErr := server.randomSourceAddress()
		if randomErr != nil {
			return nil, randomErr
		}
		for _, target := range ipv6Targets {
			dialer := net.Dialer{
				Timeout:   server.dialTimeout,
				LocalAddr: &net.TCPAddr{IP: net.IP(sourceAddress.AsSlice())},
			}
			connection, dialErr := dialer.DialContext(
				ctx,
				"tcp6",
				net.JoinHostPort(target.String(), fmt.Sprint(port)),
			)
			if dialErr == nil {
				return connection, nil
			}
			lastError = dialErr
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, lastError
	}

	for _, target := range ipv4Targets {
		dialer := net.Dialer{Timeout: server.dialTimeout}
		connection, dialErr := dialer.DialContext(
			ctx,
			"tcp4",
			net.JoinHostPort(target.String(), fmt.Sprint(port)),
		)
		if dialErr == nil {
			return connection, nil
		}
		lastError = dialErr
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}

	if lastError == nil {
		lastError = errors.New("no usable addresses")
	}
	return nil, lastError
}

func (server *dialServer) resolvePublicTargets(ctx context.Context, host string) ([]netip.Addr, []netip.Addr, error) {
	if parsed, err := netip.ParseAddr(host); err == nil {
		parsed = parsed.Unmap()
		if server.isDisallowed(parsed) {
			return nil, nil, errDestinationNotAllowed
		}
		if parsed.Is6() {
			return []netip.Addr{parsed}, nil, nil
		}
		return nil, []netip.Addr{parsed}, nil
	}

	resolved, err := server.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, nil, err
	}
	ipv6Targets := make([]netip.Addr, 0, len(resolved))
	ipv4Targets := make([]netip.Addr, 0, len(resolved))
	for _, address := range resolved {
		address = address.Unmap()
		if server.isDisallowed(address) {
			continue
		}
		if address.Is6() {
			ipv6Targets = append(ipv6Targets, address)
		} else if address.Is4() {
			ipv4Targets = append(ipv4Targets, address)
		}
	}
	if len(ipv6Targets) == 0 && len(ipv4Targets) == 0 {
		return nil, nil, errDestinationNotAllowed
	}
	return ipv6Targets, ipv4Targets, nil
}

func (server *dialServer) isDisallowed(address netip.Addr) bool {
	return !address.IsGlobalUnicast() ||
		address.IsPrivate() ||
		address.IsLoopback() ||
		address.IsLinkLocalUnicast() ||
		server.prefix.Contains(address)
}

func (server *dialServer) randomSourceAddress() (netip.Addr, error) {
	addressBytes := server.prefix.Addr().As16()
	if _, err := cryptorand.Read(addressBytes[8:]); err != nil {
		return netip.Addr{}, err
	}
	allZero := true
	for _, value := range addressBytes[8:] {
		if value != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		addressBytes[15] = 1
	}
	return netip.AddrFrom16(addressBytes), nil
}

func writeReply(connection net.Conn, reply byte, bound net.Addr) error {
	addressType := byte(socksIPv4)
	addressBytes := make([]byte, net.IPv4len)
	var port uint16

	if tcpAddress, ok := bound.(*net.TCPAddr); ok {
		port = uint16(tcpAddress.Port)
		if ipv4 := tcpAddress.IP.To4(); ipv4 != nil {
			copy(addressBytes, ipv4)
		} else if ipv6 := tcpAddress.IP.To16(); ipv6 != nil {
			addressType = socksIPv6
			addressBytes = make([]byte, net.IPv6len)
			copy(addressBytes, ipv6)
		}
	}

	response := make([]byte, 0, 4+len(addressBytes)+2)
	response = append(response, socksVersion5, reply, 0x00, addressType)
	response = append(response, addressBytes...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, port)
	response = append(response, portBytes...)
	_, err := connection.Write(response)
	return err
}

func relay(left net.Conn, right net.Conn) {
	done := make(chan struct{}, 2)
	copyDirection := func(destination net.Conn, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if tcp, ok := destination.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyDirection(left, right)
	go copyDirection(right, left)
	<-done
	<-done
}

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
}
