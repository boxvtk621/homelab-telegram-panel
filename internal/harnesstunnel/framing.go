package harnesstunnel

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

func writeFrame(writer io.Writer, value any) error {
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > MaximumFrame {
		return errors.New("invalid tunnel frame")
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := writer.Write(header[:]); err != nil {
		return err
	}
	_, err = writer.Write(raw)
	return err
}

func readFrame(reader io.Reader, target any) error {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(header[:])
	if size == 0 || size > MaximumFrame {
		return errors.New("invalid tunnel frame")
	}
	raw := make([]byte, int(size))
	if _, err := io.ReadFull(reader, raw); err != nil || !strictjson.Valid(raw) {
		return errors.New("invalid tunnel frame")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid tunnel frame")
	}
	return nil
}

func WriteDialRequest(writer io.Writer, request DialRequest) error {
	return writeFrame(writer, request)
}
func WriteDialReply(writer io.Writer, reply DialReply) error { return writeFrame(writer, reply) }

func ReadDialRequest(reader io.Reader) (DialRequest, error) {
	var request DialRequest
	err := readFrame(reader, &request)
	return request, err
}

func ReadDialReply(reader io.Reader) (DialReply, error) {
	var reply DialReply
	err := readFrame(reader, &reply)
	if err == nil && (reply.SchemaID != ReplySchemaID || reply.Status != "ready" && reply.Status != "rejected" || reply.Status == "ready" && reply.Code != "" || reply.Status == "rejected" && !refPattern.MatchString(reply.Code)) {
		err = errors.New("invalid tunnel reply")
	}
	return reply, err
}
