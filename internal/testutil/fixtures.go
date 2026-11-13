// Package testutil holds fixtures shared by the test suites.
package testutil

// Minimal byte sequences that the content sniffer recognises. They are file
// headers, not playable media: enough to exercise the allow-list.
var (
	PNG  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89")
	JPEG = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00\x01\x01\x00\x00\x01\x00\x01\x00\x00")
	GIF  = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff")
	WebP = []byte("RIFF\x24\x00\x00\x00WEBPVP8 \x18\x00\x00\x00")
	MP3  = []byte("ID3\x03\x00\x00\x00\x00\x00\x0a\xff\xfb\x90\x00\x00\x00\x00\x00")
	WAV  = []byte("RIFF\x24\x00\x00\x00WAVEfmt \x10\x00\x00\x00\x01\x00\x01\x00")
	M4A  = []byte("\x00\x00\x00\x20ftypM4A \x00\x00\x00\x00M4A mp42isom\x00\x00\x00\x00")
	MP4  = []byte("\x00\x00\x00\x20ftypisom\x00\x00\x02\x00isomiso2avc1mp41")
	MOV  = []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00qt  \x00\x00\x00\x08wide")
	WebM = []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81\x01\x42\xf2\x81\x04\x42\xf3\x81\x08\x42\x82\x84webm\x42\x87\x81\x04")
)

// Non-media content that must never be uploaded.
var (
	PrivateKey = []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n-----END OPENSSH PRIVATE KEY-----\n")
	DotEnv     = []byte("DATABASE_URL=postgres://user:pass@db/app\nSTRIPE_SECRET_KEY=sk_live_abcdef\n")
	HTML       = []byte("<!doctype html><html><body><script>alert(1)</script></body></html>")
	SVG        = []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	ELF        = []byte("\x7fELF\x02\x01\x01\x00\x00\x00\x00\x00\x00\x00\x00\x00\x02\x00\x3e\x00\x01\x00\x00\x00")
	PDF        = []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nendobj\n")
)
