//go:build !cgo

package blemidi

import "errors"

// openALSADataPlane is unavailable in pure-Go (CGO_ENABLED=0) builds: the gomidi
// rtmidi/ALSA driver links libasound and therefore requires CGO. Such builds
// fall back to the raw-GATT data plane (gatt.go). Production release builds set
// CGO_ENABLED=1, so the ALSA-seq path in alsa.go is the one normally used.
func openALSADataPlane(string) (dataPlane, error) {
	return nil, errors.New("blemidi: ALSA-seq data plane requires a CGO build (CGO_ENABLED=1)")
}
