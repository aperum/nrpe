/*
Package nrpe is a nagios nrpe client library.
*/
package nrpe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"time"
	"unsafe"
)

var crc32Table []uint32

var randSource *rand.Rand

const (
	maxPacketDataLength = 1024
	packetLength        = maxPacketDataLength + 12
)

const (
	queryPacketType    = 1
	responsePacketType = 2
)

const (
	//currently supporting latest version2 protocol
	nrpePacketVersion2 = 2
)

// CommandStatus represents result status code
type CommandStatus int

// Result status codes
const (
	StatusOk       = 0
	StatusWarning  = 1
	StatusCritical = 2
	StatusUnknown  = 3
)

// CommandResult holds information returned from nrpe server
type CommandResult struct {
	StatusLine string
	StatusCode CommandStatus
}

type packet struct {
	packetVersion []byte
	packetType    []byte
	crc32         []byte
	statusCode    []byte
	padding       []byte
	data          []byte

	all []byte
}

// Initialization of crc32Table and randSource
func init() {
	var crc, poly, i, j uint32

	crc32Table = make([]uint32, 256)

	poly = uint32(0xEDB88320)

	for i = 0; i < 256; i++ {
		crc = i

		for j = 8; j > 0; j-- {
			if (crc & 1) != 0 {
				crc = (crc >> 1) ^ poly
			} else {
				crc >>= 1
			}
		}

		crc32Table[i] = crc
	}

	randSource = rand.New(rand.NewSource(time.Now().UnixNano()))
}

//Builds crc32 from the given input
func crc32(in []byte) uint32 {
	var crc uint32

	crc = uint32(0xFFFFFFFF)

	for _, c := range in {
		crc = ((crc >> 8) & uint32(0x00FFFFFF)) ^ crc32Table[(crc^uint32(c))&0xFF]
	}

	return (crc ^ uint32(0xFFFFFFFF))
}

//extra randomization for encryption
func randomizeBuffer(in []byte) {
	n := len(in) >> 2

	for i := 0; i < n; i++ {
		r := randSource.Uint32()

		copy(in[i<<2:(i+1)<<2], (*[4]byte)(unsafe.Pointer(&r))[:])
	}

	if len(in)%4 != 0 {
		r := randSource.Uint32()

		copy(in[n<<2:], (*[4]byte)(unsafe.Pointer(&r))[:len(in)-(n<<2)])
	}
}

// Command represents command name and arguments list
type Command struct {
	Name string
	Args []string
}

// NewCommand creates Command object
func NewCommand(name string, args ...string) Command {
	return Command{
		Name: name,
		Args: args,
	}
}

func createPacket() *packet {
	var p packet
	p.all = make([]byte, packetLength)

	p.packetVersion = p.all[0:2]
	p.packetType = p.all[2:4]
	p.crc32 = p.all[4:8]
	p.statusCode = p.all[8:10]
	p.data = p.all[10 : packetLength-2]

	return &p
}

// Run specified command
func Run(conn net.Conn, command Command, isSSL bool,
	timeout time.Duration) (*CommandResult, error) {
	var err error
	pckt := createPacket()

	be := binary.BigEndian

	randomizeBuffer(pckt.all)

	be.PutUint16(pckt.packetVersion, nrpePacketVersion2)
	be.PutUint16(pckt.packetType, queryPacketType)
	be.PutUint32(pckt.crc32, 0)
	be.PutUint16(pckt.statusCode, 0)

	if len(command.Name) >= maxPacketDataLength {
		return nil, fmt.Errorf("CommandName too long: got %d , max allowed %d",
			len(command.Name), maxPacketDataLength-1)
	}

	copy(pckt.data, []byte(command.Name))

	lastPos := len(command.Name)

	for _, arg := range command.Args {
		if (lastPos + len(arg) + 1) >= maxPacketDataLength {
			return nil, fmt.Errorf("Command too long: got %d , max allowed %d",
				lastPos+len(arg)+1, maxPacketDataLength-1)
		}
		pckt.data[lastPos] = '!'
		copy(pckt.data[lastPos+1:], []byte(arg))
		lastPos += len(arg) + 1
	}

	// need to end with 0 (random now)
	pckt.data[lastPos] = 0

	if lastPos >= maxPacketDataLength {
		return nil, fmt.Errorf(
			"nrpe: Command too long: got %d , max allowed %d",
			lastPos, maxPacketDataLength)
	}

	be.PutUint32(pckt.crc32, crc32(pckt.all))

	responsePacket := createPacket()

	if isSSL {
		if err = runSSL(conn, timeout, pckt.all, responsePacket.all); err != nil {
			return nil, err
		}
	} else {
		var l int

		if timeout > 0 {
			conn.SetDeadline(time.Now().Add(timeout))
		}

		if l, err = conn.Write(pckt.all); err != nil {
			return nil, err
		}

		if l != packetLength {
			return nil, fmt.Errorf(
				"nrpe: Error writing packet, wrote:%d, expected to be written: %d",
				l, packetLength)
		}

		if timeout > 0 {
			conn.SetDeadline(time.Now().Add(timeout))
		}

		if l, err = conn.Read(responsePacket.all); err != nil {
			return nil, err
		}

		if l != packetLength {
			return nil, fmt.Errorf(
				"nrpe: Error reading packet, got: %d, expected: %d",
				l, packetLength)
		}

		rpt := be.Uint16(responsePacket.packetType)
		if rpt != responsePacketType {
			return nil, fmt.Errorf(
				"nrpe: Error response packet type, got: %d, expected: %d",
				rpt, responsePacketType)
		}
	}

	crc := be.Uint32(responsePacket.crc32)

	be.PutUint32(responsePacket.crc32, 0)

	if crc != crc32(responsePacket.all) {
		return nil, fmt.Errorf("nrpe: Response crc didn't match")
	}

	var result CommandResult

	pos := bytes.IndexByte(responsePacket.data, 0)

	if pos != -1 {
		result.StatusLine = string(responsePacket.data[:pos])
	}

	code := be.Uint16(responsePacket.statusCode)

	switch code {
	case StatusOk, StatusWarning, StatusCritical, StatusUnknown:
		result.StatusCode = CommandStatus(code)
	default:
		return nil, fmt.Errorf("nrpe: Unknown status code %d", code)
	}

	return &result, nil
}