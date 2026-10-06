package main

import (
	"blurlconvert/blurldecrypt"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		return
	}

	if !strings.HasSuffix(os.Args[1], ".blurl") && !strings.HasSuffix(os.Args[1], ".json") {
		fmt.Println("input must be a blurl or a json")
		return
	}

	var blurl BLURL

	if strings.HasSuffix(os.Args[1], ".blurl") {
		err := parseBLURL(&blurl, string(os.Args[1]))

		if err != nil {
			fmt.Println(err)
			return
		}
	} else {
		err := parseBLURLFromJSON(&blurl, string(os.Args[1]))

		if err != nil {
			fmt.Println(err)
			return
		}
	}

	var mediaurl string
	if len(blurl.Playlists) == 1 {
		mediaurl = blurl.Playlists[0].URL
	} else {
		mediaurl = GetMediaURL(&blurl)
	}

	var key []byte

	if len(blurl.Ev) > 0 {
		decodedEV, err := base64.StdEncoding.DecodeString(blurl.Ev)
		if err != nil {
			fmt.Println("Error decoding base64:", err)
			return
		}

		parsedev, err := blurldecrypt.ParseEV(decodedEV)

		key = blurldecrypt.GetEncryptionKey("keys.bin", parsedev.Nonce, parsedev.Key[:])

		if key == nil {
			return
		}

		fmt.Printf("Key: %02x\n", key)
	}

	mediaurl, err := RemoveDuplicateUUIDPath(mediaurl)

	mpddata, err := GetPlaylistMetadataByID(mediaurl)

	if err != nil {
		fmt.Println("Error getting playlist metadata:", err)
		return
	}

	trackduration := GetPlaylistDuration(mpddata)

	if trackduration <= 0 {
		fmt.Println("Track Duration is 0 exiting!")
		return
	}

	if len(mpddata.Period.AdaptationSet) == 0 {
		fmt.Println("Playlist contains no adaptation sets! exiting.")
		return
	}
	
	tracks, err := GetPlaylistTracks(mpddata, getBaseURL(mediaurl))

	if err != nil {
		fmt.Println("Error decoding track layout:", err)
		return
	}

	numberOfSegments := GetSegmentCount(tracks[0], trackduration)

	if numberOfSegments > 0 {
		for i := range tracks {
			if tracks[i].SegmentBase == nil {
				tracks[i].Segments = int(numberOfSegments)
			}
		}

		fmt.Printf("===================================================================================\n")
		fmt.Printf("Track Segments: %.0f\n", numberOfSegments)
		fmt.Printf("Media Type: %s\n", tracks[0].MediaType)
		fmt.Printf("Media Codec: %s\n", tracks[0].Representation.Codecs)
		fmt.Printf("Sample Rate: %skHz\n", tracks[0].Representation.AudioSamplingRate)
		fmt.Printf("===================================================================================\n")

		for _, track := range tracks {
			err := track.Download(hex.EncodeToString(key))

			if err != nil {
				fmt.Println("Error Downloading Track", err)
				return
			}
		}

		MergeTracks(tracks, mpddata.Period.AdaptationSet[0].DefaultKID())

		time.Sleep(1 * time.Second)

		os.RemoveAll("./downloads")

	} else {
		fmt.Println("Invalid number of track segments! exiting.")
		return
	}
}

type PlaylistTrack struct {
	MediaType      string
	Output         string
	BaseURL        string
	Representation *Representation
	SegmentBase    *SegmentBaseTrack
	Segments int
}
func GetPlaylistTracks(mpddata *MPD, fallbackurl string) ([]PlaylistTrack, error) {
	tracks := make([]PlaylistTrack, 0, len(mpddata.Period.AdaptationSet))
	outputs := make(map[string]int)

	for i := range mpddata.Period.AdaptationSet {
		adaptation := &mpddata.Period.AdaptationSet[i]

		if len(adaptation.Representation) == 0 {
			fmt.Printf("Skipping adaptation set %s: it has no representations\n", adaptation.ID)
			continue
		}

		representation := &adaptation.Representation[0]
		mediatype := adaptation.MediaType()

		outputs[mediatype]++
		output := fmt.Sprintf("master_%s", mediatype)

		if outputs[mediatype] > 1 {
			output = fmt.Sprintf("%s_%d", output, outputs[mediatype])
		}

		track := PlaylistTrack{
			MediaType:      mediatype,
			Output:         output,
			BaseURL:        fallbackurl,
			Representation: representation,
		}

		if representation.SegmentBase != nil {
			segmentbase, err := GetSegmentBaseTrack(representation, mpddata.BaseURL, fallbackurl)

			if err != nil {
				return nil, err
			}

			track.SegmentBase = segmentbase
			track.Segments = len(segmentbase.Segments)
		}

		tracks = append(tracks, track)
	}

	if len(tracks) == 0 {
		return nil, fmt.Errorf("playlist contains no tracks")
	}

	return tracks, nil
}

func GetSegmentCount(track PlaylistTrack, trackduration float64) float64 {
	if track.SegmentBase != nil {
		return float64(track.Segments)
	}

	if track.Representation == nil {
		return 0
	}

	segmentDuration, err := strconv.ParseInt(track.Representation.SegmentTemplate.Duration, 10, 64)

	if err != nil {
		return 0
	}

	timescale, err := strconv.ParseInt(track.Representation.SegmentTemplate.Timescale, 10, 64)

	if err != nil {
		return 0
	}

	if segmentDuration <= 0 || timescale <= 0 {
		return 0
	}

	return math.Ceil(trackduration / (float64(segmentDuration) / float64(timescale)))
}

func (p PlaylistTrack) Download(key string) error {
	if p.SegmentBase != nil {
		return p.SegmentBase.Download(p.Output, key)
	}

	initialization := strings.ReplaceAll(p.Representation.SegmentTemplate.Initialization, "$RepresentationID$", p.Representation.ID)

	return HandleDownloadTrack(p.Output, float64(p.Segments), p.BaseURL, initialization, p.Representation.ID, key)
}

func MergeTracks(tracks []PlaylistTrack, kid string) {
	if len(tracks) != 2 || tracks[0].MediaType == tracks[1].MediaType {
		return
	}

	name := "master"

	if kid != "" {
		name = EncodeToBase62(kid)

		if len(name) > 8 {
			name = name[:8]
		}
	}

	Merge(fmt.Sprintf("%s.mp4", tracks[0].Output), fmt.Sprintf("%s.mp4", tracks[1].Output), name)
}
