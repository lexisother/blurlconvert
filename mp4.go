package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type ByteRange struct {
	Start uint64
	End   uint64
}

func (b ByteRange) Length() uint64 {
	if b.End < b.Start {
		return 0
	}

	return b.End - b.Start + 1
}

func (b ByteRange) String() string {
	return fmt.Sprintf("%d-%d", b.Start, b.End)
}

func ParseByteRange(value string) (ByteRange, error) {
	start, end, found := strings.Cut(strings.TrimSpace(value), "-")

	if !found {
		return ByteRange{}, fmt.Errorf("malformed byte range %q", value)
	}

	parsedstart, err := strconv.ParseUint(strings.TrimSpace(start), 10, 64)

	if err != nil {
		return ByteRange{}, fmt.Errorf("malformed byte range %q: %w", value, err)
	}

	parsedend, err := strconv.ParseUint(strings.TrimSpace(end), 10, 64)

	if err != nil {
		return ByteRange{}, fmt.Errorf("malformed byte range %q: %w", value, err)
	}

	if parsedend < parsedstart {
		return ByteRange{}, fmt.Errorf("byte range %q ends before it starts", value)
	}

	return ByteRange{Start: parsedstart, End: parsedend}, nil
}

type SegmentReference struct {
	ReferenceType uint8
	Size          uint32
	Duration      uint32
}

type SegmentIndex struct {
	ReferenceID              uint32
	Timescale                uint32
	EarliestPresentationTime uint64
	FirstOffset              uint64
	References               []SegmentReference
	Size uint64
}

func (s *SegmentIndex) SegmentDuration(reference SegmentReference) float64 {
	if s.Timescale == 0 {
		return 0
	}

	return float64(reference.Duration) / float64(s.Timescale)
}

func (s *SegmentIndex) Duration() float64 {
	if s.Timescale == 0 {
		return 0
	}

	var duration uint64

	for _, reference := range s.References {
		duration += uint64(reference.Duration)
	}

	return float64(duration) / float64(s.Timescale)
}

type MediaSegment struct {
	Number   int
	Range    ByteRange
	Duration float64
}

func (s *SegmentIndex) Segments(base uint64) []MediaSegment {
	segments := make([]MediaSegment, 0, len(s.References))
	offset := base + s.Size + s.FirstOffset

	for i, reference := range s.References {
		segments = append(segments, MediaSegment{
			Number:   i + 1,
			Range:    ByteRange{Start: offset, End: offset + uint64(reference.Size) - 1},
			Duration: s.SegmentDuration(reference),
		})

		offset += uint64(reference.Size)
	}

	return segments
}

func ParseSegmentIndex(data []byte) (*SegmentIndex, uint64, error) {
	for offset := uint64(0); offset+8 <= uint64(len(data)); {
		size, header, err := boxSize(data, offset)
		if err != nil {
			return nil, 0, err
		}

		if string(data[offset+4:offset+8]) == "sidx" {
			index, err := parseSegmentIndexBox(data[offset+header : offset+size])

			if err != nil {
				return nil, 0, err
			}

			index.Size = size

			return index, offset, nil
		}

		offset += size
	}

	return nil, 0, errors.New("no segment index (sidx) box found")
}

func boxSize(data []byte, offset uint64) (uint64, uint64, error) {
	size := uint64(binary.BigEndian.Uint32(data[offset : offset+4]))
	header := uint64(8)

	switch size {
	case 0:
		// a size of 0 means the box runs until the end of the data
		size = uint64(len(data)) - offset
	case 1:
		if offset+16 > uint64(len(data)) {
			return 0, 0, errors.New("truncated box header")
		}

		size = binary.BigEndian.Uint64(data[offset+8 : offset+16])
		header = 16
	}

	if size < header || offset+size > uint64(len(data)) {
		return 0, 0, fmt.Errorf("truncated box at offset %d", offset)
	}

	return size, header, nil
}

func parseSegmentIndexBox(payload []byte) (*SegmentIndex, error) {
	if len(payload) < 12 {
		return nil, errors.New("truncated segment index box")
	}

	version := payload[0]

	if version > 1 {
		return nil, fmt.Errorf("unsupported segment index version %d", version)
	}

	index := &SegmentIndex{}
	offset := 4

	index.ReferenceID = binary.BigEndian.Uint32(payload[offset : offset+4])
	offset += 4
	index.Timescale = binary.BigEndian.Uint32(payload[offset : offset+4])
	offset += 4

	if version == 0 {
		index.EarliestPresentationTime = uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		offset += 4
		index.FirstOffset = uint64(binary.BigEndian.Uint32(payload[offset : offset+4]))
		offset += 4
	} else {
		index.EarliestPresentationTime = binary.BigEndian.Uint64(payload[offset : offset+8])
		offset += 8
		index.FirstOffset = binary.BigEndian.Uint64(payload[offset : offset+8])
		offset += 8
	}

	offset += 2 // reserved
	referencecount := int(binary.BigEndian.Uint16(payload[offset : offset+2]))
	offset += 2

	if len(payload) < offset+referencecount*12 {
		return nil, errors.New("truncated segment index reference list")
	}

	index.References = make([]SegmentReference, 0, referencecount)

	for i := 0; i < referencecount; i++ {
		index.References = append(index.References, SegmentReference{
			ReferenceType: payload[offset] >> 7,
			Size:          binary.BigEndian.Uint32(payload[offset:offset+4]) & 0x7fffffff,
			Duration:      binary.BigEndian.Uint32(payload[offset+4 : offset+8]),
		})

		offset += 12
	}

	return index, nil
}

func fetchRange(resourceurl string, requested ByteRange) ([]byte, error) {
	request, err := http.NewRequest(http.MethodGet, resourceurl, nil)

	if err != nil {
		return nil, err
	}

	request.Header.Set("Range", fmt.Sprintf("bytes=%s", requested))

	response, err := http.DefaultClient.Do(request)

	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("bad status while downloading bytes=%s: %s", requested, response.Status)
	}

	body, err := io.ReadAll(response.Body)

	if err != nil {
		return nil, err
	}

	if response.StatusCode == http.StatusOK && uint64(len(body)) != requested.Length() {
		if uint64(len(body)) < requested.End+1 {
			return nil, fmt.Errorf("resource is shorter than bytes=%s requested (%d bytes)", requested, len(body))
		}

		body = body[requested.Start : requested.End+1]
	}

	if uint64(len(body)) != requested.Length() {
		return nil, fmt.Errorf("expected %d bytes for bytes=%s but got %d", requested.Length(), requested, len(body))
	}

	return body, nil
}

type SegmentBaseTrack struct {
	URL            string
	Name           string
	Initialization ByteRange
	Index          ByteRange
	IndexData      []byte
	IndexOffset    uint64
	IndexSize      uint64
	Segments       []MediaSegment
	Timescale      uint32
	Duration       float64
}

func GetSegmentBaseTrack(representation *Representation, mpdbaseurl string, fallbackurl string) (*SegmentBaseTrack, error) {
	if representation.SegmentBase == nil {
		return nil, errors.New("representation has no SegmentBase")
	}

	segmentbase := representation.SegmentBase

	initialization, err := ParseByteRange(segmentbase.Initialization.Range)

	if err != nil {
		return nil, fmt.Errorf("invalid initialization range: %w", err)
	}

	index, err := ParseByteRange(segmentbase.IndexRange)

	if err != nil {
		return nil, fmt.Errorf("invalid index range: %w", err)
	}

	resourceurl := representation.MediaURL(mpdbaseurl, fallbackurl)

	track := &SegmentBaseTrack{
		URL:            resourceurl,
		Name:           mediaResourceName(resourceurl),
		Initialization: initialization,
		Index:          index,
	}

	fmt.Printf("Initializing %s [%s]\n", track.URL, initialization)

	track.IndexData, err = fetchRange(track.URL, index)

	if err != nil {
		return nil, fmt.Errorf("error downloading segment index: %w", err)
	}

	segmentindex, indexoffset, err := ParseSegmentIndex(track.IndexData)

	if err != nil {
		return nil, fmt.Errorf("error decoding segment index %s: %w", index, err)
	}

	track.IndexOffset = indexoffset
	track.IndexSize = segmentindex.Size
	track.Segments = segmentindex.Segments(index.Start + indexoffset)
	track.Timescale = segmentindex.Timescale
	track.Duration = segmentindex.Duration()

	return track, nil
}

func (t *SegmentBaseTrack) Download(output string, key string) error {
	if len(t.Segments) == 0 {
		return errors.New("segment index is empty")
	}

	if !isDirExists("downloads") {
		if err := os.Mkdir("downloads", 0755); err != nil {
			return err
		}
	}

	initialization, err := fetchRange(t.URL, t.Initialization)

	if err != nil {
		return fmt.Errorf("error downloading init segment: %w", err)
	}

	mediarange := ByteRange{Start: t.Segments[0].Range.Start, End: t.Segments[len(t.Segments)-1].Range.End}

	fmt.Printf("Downloading %s [%s]\n", t.URL, mediarange)

	mediadata, err := fetchRange(t.URL, mediarange)

	if err != nil {
		return fmt.Errorf("error downloading media segments: %w", err)
	}

	index := t.IndexData[t.IndexOffset : t.IndexOffset+t.IndexSize]

	if err = writeSegmentBaseTrack(t.Name, initialization, index, mediadata, mediarange, t.Segments); err != nil {
		return err
	}

	return finalizeTrack(output, t.Name, key)
}

func writeSegmentBaseTrack(name string, initialization []byte, index []byte, mediadata []byte, mediarange ByteRange, segments []MediaSegment) error {
	assembled, err := os.Create(filepath.Join("downloads", name))

	if err != nil {
		return err
	}

	defer assembled.Close()

	if _, err = assembled.Write(initialization); err != nil {
		return err
	}

	if _, err = assembled.Write(index); err != nil {
		return err
	}

	for _, segment := range segments {
		start := segment.Range.Start - mediarange.Start
		end := start + segment.Range.Length()

		if end > uint64(len(mediadata)) {
			return fmt.Errorf("segment %d lies outside of the downloaded media data", segment.Number)
		}

		if _, err = assembled.Write(mediadata[start:end]); err != nil {
			return err
		}
	}

	return nil
}

func mediaResourceName(resourceurl string) string {
	name := resourceurl

	if parsed, err := url.Parse(resourceurl); err == nil {
		name = parsed.Path
	}

	if unescaped, err := url.PathUnescape(path.Base(name)); err == nil {
		name = unescaped
	}

	name = filepath.Base(name)

	if name == "." || name == string(filepath.Separator) || name == "" {
		return "track.mp4"
	}

	return name
}
