package schema

import (
	"errors"
	"strconv"
)

//go:generate stringer -type=Method -linecomment

// Archive represents a metric archive
// the zero value represents a raw metric
// any non-zero value represents a certain
// aggregation method (lower 4 bits) and
// aggregation span (higher 4 bits)
type Archive uint8

// important: caller must make sure to call IsSpanValid first
func NewArchive(method Method, span uint32) Archive {
	code := spanHumanToCode[span]
	return Archive(uint8(method) | code<<4)
}

// String returns the traditional key suffix like sum_600 etc
// (invalid to call this for raw archives)
func (a Archive) String() string {
	return a.Method().String() + "_" + strconv.FormatInt(int64(a.Span()), 10)
}

func (a Archive) Method() Method {
	return Method(a & 0x0F)
}

func (a Archive) Span() uint32 {
	return spanCodeToHuman[uint8(a>>4)]
}

func IsSpanValid(span uint32) bool {
	_, ok := spanHumanToCode[span]
	return ok
}

type Method uint8

const (
	Avg Method = iota + 1 // avg
	Sum                   // sum
	Lst                   // lst
	Max                   // max
	Min                   // min
	Cnt                   // cnt
)

func MethodFromString(input string) (Method, error) {
	switch input {
	case "avg":
		return Avg, nil
	case "sum":
		return Sum, nil
	case "lst":
		return Lst, nil
	case "max":
		return Max, nil
	case "min":
		return Min, nil
	case "cnt":
		return Cnt, nil
	}
	return 0, errors.New("no such method")
}

// maps human friendly span numbers (in seconds) to optimized code form
var spanHumanToCode map[uint32]uint8

// maps span codes to human friendly span numbers in seconds
var spanCodeToHuman map[uint8]uint32

func init() {
	// all the aggregation spans we support, their index position in this slice is their code
	spans := []uint32{2, 5, 10, 15, 30, 60, 90, 120, 150, 300, 600, 900, 1200, 1800, 45 * 60, 3600, 3600 + 30*60, 2 * 3600, 3 * 3600, 4 * 3600, 5 * 3600, 6 * 3600}

	spanHumanToCode = make(map[uint32]uint8)
	spanCodeToHuman = make(map[uint8]uint32)

	for i, human := range spans {
		code := uint8(i)
		spanHumanToCode[human] = code
		spanCodeToHuman[code] = human
	}

}
