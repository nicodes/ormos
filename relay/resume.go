package relay

import (
	"encoding/binary"
	"errors"
	"math"
)

// Acknowledged terminal framing is distinct from the existing v4 resource
// identity contract and seal-v2 key schedule. Callers must negotiate it before
// using these messages; adding the codec does not advertise that capability.
const (
	resumeHelloTag     termTag = 3
	resumeDataTag      termTag = 4
	resumeInputAckTag  termTag = 5
	resumeGapTag       termTag = 6
	resumeReadyTag     termTag = 7
	resumeClientAckTag termTag = 8
)

type ResumeHello struct {
	Writer         [16]byte
	OutputOffset   uint64
	InputConfirmed uint64
	Initial        bool
}

type ResumeReady struct {
	InputAccepted uint64
	InputWritten  uint64
	OutputOffset  uint64
	Initial       bool
}

type SequencedData struct {
	Offset uint64
	Data   []byte
}
type ResumeClientAck struct{ OutputOffset, InputConfirmed uint64 }
type ReplayGap struct {
	Reason        byte
	AvailableFrom uint64
}

const (
	ReplayOutputExpired byte = 1
	ReplayWriterUnknown byte = 2
	ReplayInputConflict byte = 3
	ReplayCapacity      byte = 4
)

type ResumeFrame struct {
	Hello     *ResumeHello
	Ready     *ResumeReady
	Data      *SequencedData
	InputAck  *uint64
	ClientAck *ResumeClientAck
	Gap       *ReplayGap
	Resize    *Resize
	Active    *bool
}

func EncodeResumeHello(value ResumeHello) []byte {
	payload := make([]byte, 33)
	copy(payload, value.Writer[:])
	binary.BigEndian.PutUint64(payload[16:], value.OutputOffset)
	binary.BigEndian.PutUint64(payload[24:], value.InputConfirmed)
	if value.Initial {
		payload[32] = 1
	}
	return encodeFrame(resumeHelloTag, payload)
}

func EncodeResumeReady(value ResumeReady) ([]byte, error) {
	if value.InputWritten > value.InputAccepted {
		return nil, errors.New("written input exceeds accepted input")
	}
	payload := make([]byte, 25)
	binary.BigEndian.PutUint64(payload, value.InputAccepted)
	binary.BigEndian.PutUint64(payload[8:], value.InputWritten)
	binary.BigEndian.PutUint64(payload[16:], value.OutputOffset)
	if value.Initial {
		payload[24] = 1
	}
	return encodeFrame(resumeReadyTag, payload), nil
}

func EncodeSequencedData(offset uint64, data []byte) ([]byte, error) {
	if len(data) > MaxFrameSize-8 || uint64(len(data)) > math.MaxUint64-offset {
		return nil, errors.New("sequenced data exceeds its bound")
	}
	payload := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(payload, offset)
	copy(payload[8:], data)
	return encodeFrame(resumeDataTag, payload), nil
}

func EncodeInputAck(written uint64) []byte {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, written)
	return encodeFrame(resumeInputAckTag, payload)
}
func EncodeClientAck(value ResumeClientAck) []byte {
	payload := make([]byte, 16)
	binary.BigEndian.PutUint64(payload, value.OutputOffset)
	binary.BigEndian.PutUint64(payload[8:], value.InputConfirmed)
	return encodeFrame(resumeClientAckTag, payload)
}
func EncodeReplayGap(value ReplayGap) ([]byte, error) {
	if value.Reason < ReplayOutputExpired || value.Reason > ReplayCapacity {
		return nil, errors.New("unknown replay gap")
	}
	payload := make([]byte, 9)
	payload[0] = value.Reason
	binary.BigEndian.PutUint64(payload[1:], value.AvailableFrom)
	return encodeFrame(resumeGapTag, payload), nil
}

func DecodeResumeFrame(data []byte) (ResumeFrame, error) {
	invalid := errors.New("invalid acknowledged terminal frame")
	if len(data) < frameHeaderSize {
		return ResumeFrame{}, invalid
	}
	size := binary.BigEndian.Uint32(data[1:frameHeaderSize])
	if size > MaxFrameSize || uint64(size)+frameHeaderSize != uint64(len(data)) {
		return ResumeFrame{}, invalid
	}
	payload := data[frameHeaderSize:]
	switch termTag(data[0]) {
	case resumeHelloTag:
		if len(payload) != 33 || payload[32] > 1 {
			return ResumeFrame{}, invalid
		}
		value := &ResumeHello{OutputOffset: binary.BigEndian.Uint64(payload[16:]), InputConfirmed: binary.BigEndian.Uint64(payload[24:]), Initial: payload[32] == 1}
		copy(value.Writer[:], payload[:16])
		if value.Writer == ([16]byte{}) {
			return ResumeFrame{}, invalid
		}
		return ResumeFrame{Hello: value}, nil
	case resumeReadyTag:
		if len(payload) != 25 || payload[24] > 1 {
			return ResumeFrame{}, invalid
		}
		value := &ResumeReady{InputAccepted: binary.BigEndian.Uint64(payload), InputWritten: binary.BigEndian.Uint64(payload[8:]), OutputOffset: binary.BigEndian.Uint64(payload[16:]), Initial: payload[24] == 1}
		if value.InputWritten > value.InputAccepted {
			return ResumeFrame{}, invalid
		}
		return ResumeFrame{Ready: value}, nil
	case resumeDataTag:
		if len(payload) < 8 {
			return ResumeFrame{}, invalid
		}
		offset := binary.BigEndian.Uint64(payload)
		if uint64(len(payload)-8) > math.MaxUint64-offset {
			return ResumeFrame{}, invalid
		}
		return ResumeFrame{Data: &SequencedData{Offset: offset, Data: payload[8:]}}, nil
	case resumeInputAckTag:
		if len(payload) != 8 {
			return ResumeFrame{}, invalid
		}
		value := binary.BigEndian.Uint64(payload)
		return ResumeFrame{InputAck: &value}, nil
	case resumeClientAckTag:
		if len(payload) != 16 {
			return ResumeFrame{}, invalid
		}
		return ResumeFrame{ClientAck: &ResumeClientAck{OutputOffset: binary.BigEndian.Uint64(payload), InputConfirmed: binary.BigEndian.Uint64(payload[8:])}}, nil
	case resumeGapTag:
		if len(payload) != 9 || payload[0] < ReplayOutputExpired || payload[0] > ReplayCapacity {
			return ResumeFrame{}, invalid
		}
		return ResumeFrame{Gap: &ReplayGap{Reason: payload[0], AvailableFrom: binary.BigEndian.Uint64(payload[1:])}}, nil
	case tagResize, tagActivity:
		legacy, err := DecodeFrame(data)
		return ResumeFrame{Resize: legacy.Resize, Active: legacy.Active}, err
	default:
		return ResumeFrame{}, invalid // unsequenced data cannot bypass acknowledgments
	}
}
