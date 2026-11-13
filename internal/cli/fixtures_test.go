package cli

import (
	"encoding/json"

	"github.com/geniuspath/apipod-cli/internal/testutil"
)

var (
	pngFixture = testutil.PNG
	mp4Fixture = testutil.MP4
	mp3Fixture = testutil.MP3
)

func jsonUnmarshal(s string, into any) error { return json.Unmarshal([]byte(s), into) }
