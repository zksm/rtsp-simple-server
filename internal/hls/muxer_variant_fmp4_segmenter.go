package hls

import (
	"bytes"
	"fmt"
	"time"

	"github.com/aler9/gortsplib"
	"github.com/aler9/gortsplib/pkg/h264"
)

type muxerVariantFMP4Segmenter struct {
	lowLatency         bool
	segmentDuration    time.Duration
	segmentMaxSize     uint64
	videoTrack         *gortsplib.TrackH264
	audioTrack         *gortsplib.TrackAAC
	onSegmentFinalized func(*muxerVariantFMP4Segment)
	onPartFinalized    func(*muxerVariantFMP4Part)

	currentSegment *muxerVariantFMP4Segment
	videoStartPTS  time.Duration
	audioStartPTS  *time.Duration
	startTime      time.Time
	lastSPS        []byte
	lastPPS        []byte
	nextSegmentID  uint64
	nextPartID     uint64
}

func newMuxerVariantFMP4Segmenter(
	lowLatency bool,
	segmentDuration time.Duration,
	segmentMaxSize uint64,
	videoTrack *gortsplib.TrackH264,
	audioTrack *gortsplib.TrackAAC,
	onSegmentFinalized func(*muxerVariantFMP4Segment),
	onPartFinalized func(*muxerVariantFMP4Part),
) *muxerVariantFMP4Segmenter {
	m := &muxerVariantFMP4Segmenter{
		lowLatency:         lowLatency,
		segmentDuration:    segmentDuration,
		segmentMaxSize:     segmentMaxSize,
		videoTrack:         videoTrack,
		audioTrack:         audioTrack,
		onSegmentFinalized: onSegmentFinalized,
		onPartFinalized:    onPartFinalized,
	}

	return m
}

func (m *muxerVariantFMP4Segmenter) reset() {
	_, err := m.currentSegment.finalize()
	if err == nil {
		m.onSegmentFinalized(m.currentSegment)
	}

	m.currentSegment = nil
	m.videoStartPTS = 0
	m.startTime = time.Time{}
	m.lastSPS = nil
	m.lastPPS = nil
	m.nextSegmentID = 0
	m.nextPartID = 0
}

func (m *muxerVariantFMP4Segmenter) genSegmentID() uint64 {
	id := m.nextSegmentID
	m.nextSegmentID++
	return id
}

func (m *muxerVariantFMP4Segmenter) genPartID() uint64 {
	id := m.nextPartID
	m.nextPartID++
	return id
}

func (m *muxerVariantFMP4Segmenter) writeH264(pts time.Duration, nalus [][]byte) error {
	now := time.Now()
	idrPresent := h264.IDRPresent(nalus)

	if m.currentSegment == nil {
		// skip groups silently until we find one with a IDR
		if !idrPresent {
			return nil
		}

		// create first segment
		var err error
		m.currentSegment, err = newMuxerVariantFMP4Segment(
			m.lowLatency,
			m.genSegmentID(),
			now,
			0,
			m.segmentMaxSize,
			m.videoTrack,
			m.audioTrack,
			m.genPartID,
			m.onPartFinalized,
		)
		if err != nil {
			return err
		}

		m.lastSPS = m.videoTrack.SPS()
		m.lastPPS = m.videoTrack.PPS()
		m.videoStartPTS = pts
		m.startTime = time.Now()
		pts = 0
	} else {
		pts -= m.videoStartPTS

		// switch segment
		if idrPresent {
			sps := m.videoTrack.SPS()
			pps := m.videoTrack.PPS()

			if (pts-m.currentSegment.startDTS) >= m.segmentDuration ||
				!bytes.Equal(m.lastSPS, sps) ||
				!bytes.Equal(m.lastPPS, pps) {
				residualAudioEntries, err := m.currentSegment.finalize()
				if err != nil {
					return err
				}
				m.onSegmentFinalized(m.currentSegment)

				m.lastSPS = sps
				m.lastPPS = pps
				m.currentSegment, err = newMuxerVariantFMP4Segment(
					m.lowLatency,
					m.genSegmentID(),
					now,
					pts,
					m.segmentMaxSize,
					m.videoTrack,
					m.audioTrack,
					m.genPartID,
					m.onPartFinalized,
				)
				if err != nil {
					return err
				}

				for _, entry := range residualAudioEntries {
					fmt.Println("RES PTS", entry.pts)
					err := m.currentSegment.writeAAC(entry.pts, [][]byte{entry.au})
					if err != nil {
						return err
					}
				}
			}
		}
	}

	err := m.currentSegment.writeH264(pts, nalus)
	if err != nil {
		m.reset()
		return err
	}

	return nil
}

func (m *muxerVariantFMP4Segmenter) writeAAC(pts time.Duration, aus [][]byte) error {
	now := time.Now()

	if m.videoTrack == nil {
		if m.currentSegment == nil {
			// create first segment
			var err error
			m.currentSegment, err = newMuxerVariantFMP4Segment(
				m.lowLatency,
				m.genSegmentID(),
				now,
				0,
				m.segmentMaxSize,
				m.videoTrack,
				m.audioTrack,
				m.genPartID,
				m.onPartFinalized,
			)
			if err != nil {
				return err
			}

			v := pts
			m.audioStartPTS = &v
			m.startTime = time.Now()
			pts = 0
		} else {
			pts -= *m.audioStartPTS

			// switch segment
			if m.currentSegment.audioEntriesCount >= segmentMinAUCount &&
				(pts-m.currentSegment.startDTS) >= m.segmentDuration {
				_, err := m.currentSegment.finalize()
				if err != nil {
					return err
				}
				m.onSegmentFinalized(m.currentSegment)

				m.currentSegment, err = newMuxerVariantFMP4Segment(
					m.lowLatency,
					m.genSegmentID(),
					now,
					pts,
					m.segmentMaxSize,
					m.videoTrack,
					m.audioTrack,
					m.genPartID,
					m.onPartFinalized,
				)
				if err != nil {
					return err
				}
			}
		}
	} else {
		// wait for the video track
		if m.currentSegment == nil {
			return nil
		}

		if m.audioStartPTS == nil {
			v := m.videoStartPTS - time.Now().Sub(m.startTime)
			m.audioStartPTS = &v
		}

		pts -= *m.audioStartPTS

		if pts < m.currentSegment.currentPart.startDTS {
			fmt.Println("HEEEEERE3", m.currentSegment.currentPart.startDTS-pts)
			*m.audioStartPTS -= m.currentSegment.currentPart.startDTS - pts
			pts = m.currentSegment.currentPart.startDTS
		}
	}

	err := m.currentSegment.writeAAC(pts, aus)
	if err != nil {
		m.reset()
		return err
	}

	return nil
}
