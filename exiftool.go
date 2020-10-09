package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ExifToolMetadata struct {
	SourceFile  string
	DataMap     KeyValueMap
	DataMapJson string
}

type ExiftoolConfig struct {
	executable       string
	bufferOpenArgs   []string
	readyToken       []byte
	readyTokenLength int
	bufferCloseArgs  []string
	dataExtractArgs  []string
	executeArg       string
}

type ExiftoolIO struct {
	lock          sync.Mutex
	stdin         io.WriteCloser
	stdMergedOut  io.ReadCloser
	scanMergedOut *bufio.Scanner
	bufferSet     bool
	buffer        []byte
	bufferMaxSize int
}

type Exiftool struct {
	config ExiftoolConfig
	io     ExiftoolIO
}

func (et *Exiftool) UseDefaults() {
	var readyToken []byte

	if runtime.GOOS == "windows" {
		readyToken = []byte("{ready}\r\n")
	} else {
		readyToken = []byte("{ready}\n")
	}

	et.config = ExiftoolConfig{
		executable:       "exiftool",
		bufferOpenArgs:   []string{"-stay_open", "True", "-@", "-", "-common_args"},
		readyToken:       readyToken,
		readyTokenLength: len(readyToken),
		bufferCloseArgs:  []string{"-stay_open", "False", "-execute"},
		dataExtractArgs: []string{
			"-json",
			"-api", "largefilesupport=1",
			"-extractEmbedded",
			// exclude these tags (which can be very big strings)
			"-x", "HistoryChanged",
			"-x", "HistoryWhen",
			"-x", "HistorySoftwareAgent",
			"-x", "HistoryInstanceID",
			"-x", "HistoryAction",
			"-x", "ThumbnailImage",
		},
		executeArg: "-execute",
	}
}

func (et *Exiftool) Open() error {
	if et.config.executable == "" {
		et.UseDefaults()
	}

	cmd := exec.Command(et.config.executable, et.config.bufferOpenArgs...)
	r, w := io.Pipe()
	et.io.stdMergedOut = r

	cmd.Stdout = w
	cmd.Stderr = w

	var err error
	if et.io.stdin, err = cmd.StdinPipe(); err != nil {
		return fmt.Errorf("error when piping stdin: %w", err)
	}

	et.io.scanMergedOut = bufio.NewScanner(r)
	if et.io.bufferSet {
		et.io.scanMergedOut.Buffer(et.io.buffer, et.io.bufferMaxSize)
	}
	et.io.scanMergedOut.Split(et.splitReadyToken)

	if err = cmd.Start(); err != nil {
		return fmt.Errorf("error when executing commande: %w", err)
	}

	return nil
}

func (et *Exiftool) splitReadyToken(data []byte, atEOF bool) (int, []byte, error) {
	idx := bytes.Index(data, et.config.readyToken)
	if idx == -1 {
		if atEOF && len(data) > 0 {
			return 0, data, fmt.Errorf("no final token found")
		}

		return 0, nil, nil
	}

	return idx + et.config.readyTokenLength, data[:idx], nil
}

// Close closes exiftool. If anything went wrong, a non empty error will be returned
func (et *Exiftool) Close() error {
	et.io.lock.Lock()
	defer et.io.lock.Unlock()

	for _, v := range et.config.bufferCloseArgs {
		_, err := fmt.Fprintln(et.io.stdin, v)
		if err != nil {
			return err
		}
	}

	var errs []error
	if err := et.io.stdMergedOut.Close(); err != nil {
		errs = append(errs, fmt.Errorf("error while closing stdMergedOut: %w", err))
	}

	if err := et.io.stdin.Close(); err != nil {
		errs = append(errs, fmt.Errorf("error while closing stdin: %w", err))
	}

	if len(errs) > 0 {
		return fmt.Errorf("error while closing exiftool: %v", errs)
	}

	return nil
}

// ExtractMetadata extracts metadata from files
func (et *Exiftool) ReadMetadata(file string) (ExifToolMetadata, error) {
	et.io.lock.Lock()
	defer et.io.lock.Unlock()

	meta := ExifToolMetadata{}
	meta.SourceFile = file
	meta.Parse([]byte("{}"))

	for _, dataExtractArg := range et.config.dataExtractArgs {
		fmt.Fprintln(et.io.stdin, dataExtractArg)
	}

	fmt.Fprintln(et.io.stdin, file)
	fmt.Fprintln(et.io.stdin, et.config.executeArg)

	if !et.io.scanMergedOut.Scan() {
		return meta, fmt.Errorf("error reading exif data:" + file)
	}

	if et.io.scanMergedOut.Err() != nil {
		return meta, fmt.Errorf("error while reading stdMergedOut: %w", et.io.scanMergedOut.Err())
	}

	err := meta.Parse(et.io.scanMergedOut.Bytes())

	return meta, err
}

func (meta *ExifToolMetadata) Get(key string) string {
	return meta.DataMap.GetString(key)
}

func (meta *ExifToolMetadata) GetInt(key string) int {
	return meta.DataMap.GetInt(key)
}

var ZeroDateError = fmt.Errorf("zero-date string")

func (meta *ExifToolMetadata) GetTime(key string) (time.Time, error) {
	var val = meta.Get(key)
	if val == "" || !regexp.MustCompile("(?i)^[1-9]").MatchString(val) {
		return time.Time{}, ZeroDateError
	}
	dateFormat, normalizedDate := NormalizeTimestampStringFormat(val)
	date, err := time.Parse(dateFormat, normalizedDate)

	if IsError(err) {
		return date, fmt.Errorf("\n\nparsing time error formatting '%s' as '%s' \n%s", normalizedDate, dateFormat, err.Error())
	}

	return date, nil
}

func (meta *ExifToolMetadata) Parse(jsonBytes []byte) error {
	var metaMap = []KeyValueMap{}

	if err := json.Unmarshal(jsonBytes, &metaMap); err != nil {
		return fmt.Errorf("error during unmarshaling (%v): %w)", string(jsonBytes), err)
	}

	meta.DataMapJson = string(jsonBytes)
	meta.DataMap = metaMap[0]

	return nil
}

func (meta *ExifToolMetadata) GetMimeType() string {
	return meta.Get("MIMEType")
}

func (meta *ExifToolMetadata) GetGPSData() GPSData {
	gps := GPSData{}
	gps.Parse(meta.Get("GPSPosition"), meta.Get("GPSAltitude"), meta.Get("GPSDateTime"))
	return gps
}

var ExifCreationDateNotFoundError = fmt.Errorf("cannot find a suitable exif creation date")

func (meta *ExifToolMetadata) GetEarliestCreationDate() time.Time {
	var candidates []time.Time
	var candidateKeys = []string{
		"CreateDate", "ModifyDate", "DateTimeOriginal", "DateTimeDigitized", "GPSDateTime", "FileModifyDate",
	}

	fallback := time.Time{}

	for _, key := range candidateKeys {
		var candidate, err = meta.GetTime(key)
		if errors.Is(err, ZeroDateError) {
			continue
		}

		if IsError(err) {
			continue
		}
		candidates = append(candidates, candidate)
	}

	if len(candidates) == 0 {
		PrintLnRed(meta.SourceFile)
		Catch(ExifCreationDateNotFoundError)
	}

	earliest := FindEarliestDate(candidates, fallback)

	if earliest.Year() <= 1970 {
		PrintLnRed(meta.SourceFile)
		Catch(ExifCreationDateNotFoundError)
	}

	return earliest
}

func (meta *ExifToolMetadata) GetMediaWidth() int {
	return meta.GetInt("ImageWidth")
}

func (meta *ExifToolMetadata) GetMediaHeight() int {
	return meta.GetInt("ImageHeight")
}

func (meta *ExifToolMetadata) GetMediaDpi() int {
	var dpi = meta.GetInt("XResolution")
	if dpi != 0 {
		return dpi
	}
	return meta.GetInt("YResolution")
}

func (meta *ExifToolMetadata) GetMediaDuration() string {
	var candidates = []string{
		meta.Get("Duration"),
		meta.Get("MediaDuration"),
		meta.Get("TrackDuration"),
	}

	for _, val := range candidates {
		val = strings.ReplaceAll(val, "(approx)", "")
		val = strings.ReplaceAll(val, " ", "")

		if val != "" {
			return strings.TrimSpace(val)
		}
	}

	return ""
}

func (meta *ExifToolMetadata) GetFullCreationSoftware() string {
	var str = ""

	if meta.Get("CreatorTool") != "" {
		str += meta.Get("CreatorTool")
	}

	if meta.Get("Software") != "" && meta.Get("Software") != str {
		if str == "" {
			str = meta.Get("Software")
		} else {
			str += fmt.Sprintf(" (%s)", meta.Get("Software"))
		}
	}

	return strings.TrimSpace(str)
}

func (meta *ExifToolMetadata) GetFullCameraName() string {
	var str = ""

	if meta.Get("Make") != "" {
		str = meta.Get("Make")
	}

	if meta.Get("Model") != "" && meta.Get("Model") != str {
		if str == "" {
			str = meta.Get("Model")
		} else {
			str += fmt.Sprintf(" (%s)", meta.Get("Model"))
		}
	}

	return strings.TrimSpace(str)
}