package edge

import (
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type Coordinate struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type building struct {
	Lat float64
	Lon float64
}

// CQUPT teaching buildings in GCJ-02 coordinate system. x=lon, y=lat.
var teachingBuildings = map[byte]building{
	'1': {29.531049, 106.605647},
	'2': {29.532345, 106.606620},
	'3': {29.535101, 106.609243},
	'4': {29.536307, 106.609269},
	'5': {29.536018, 106.610354},
	'8': {29.534461, 106.611013},
	'9': {29.525971, 106.606189},
}

type namedBuilding struct {
	Keyword string
	Lat     float64
	Lon     float64
}

var otherBuildings = []namedBuilding{
	{"综合实验楼A", 29.525598, 106.605528},
	{"综合实验楼B", 29.525013, 106.605611},
	{"综合实验楼C", 29.524309, 106.605629},
	{"桂花篮球场", 29.530162, 106.607208},
	{"灯光篮球场", 29.532465, 106.608514},
	{"风华运动场", 29.532786, 106.607568},
	{"太极运动场", 29.532896, 106.609731},
}

var qrDataRegexp = regexp.MustCompile(`(?i)^[a-f0-9]{42}$`)
var qrURLRegexp = regexp.MustCompile(`!3~([a-fA-F0-9]+)`)

// GetLocationCoords returns coordinates for a given location name, with random jitter (~20m).
func GetLocationCoords(locationName string) *Coordinate {
	if locationName == "" {
		return nil
	}

	// 1. Check for 4-digit room number: first digit = building number
	if len(locationName) == 4 && isAllDigits(locationName) {
		if b, ok := teachingBuildings[locationName[0]]; ok {
			return applyJitter(b.Lat, b.Lon)
		}
	}

	// 2. Keyword matching against other buildings
	for _, b := range otherBuildings {
		if strings.Contains(locationName, b.Keyword) {
			return applyJitter(b.Lat, b.Lon)
		}
	}

	return nil
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func applyJitter(lat, lon float64) *Coordinate {
	jitterLat := (rand.Float64() - 0.2) * 0.0008
	jitterLon := (rand.Float64() - 0.2) * 0.0008
	return &Coordinate{
		Lat: lat + jitterLat,
		Lon: lon + jitterLon,
	}
}

// ExtractQRData validates and extracts the 42-char hex payload from a QR code string.
// Returns empty string if invalid or expired (>15s).
func ExtractQRData(rawData string) string {
	data := rawData

	// Extract from URL format: /j?p=...!3~<hex>!4~...
	if strings.HasPrefix(data, "/j?p=") || strings.Contains(data, "!3~") {
		matches := qrURLRegexp.FindStringSubmatch(data)
		if len(matches) > 1 {
			data = matches[1]
		} else {
			return ""
		}
	}

	data = strings.ToLower(data)

	if !qrDataRegexp.MatchString(data) {
		return ""
	}

	// First 10 chars are unix timestamp
	ts, err := strconv.ParseInt(data[:10], 10, 64)
	if err != nil {
		return ""
	}

	if time.Now().Unix()-ts > 15 {
		return ""
	}

	return data
}
