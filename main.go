package main

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

func getPortFromEnv(key string) (int, bool) {
	value, exists := os.LookupEnv(key)
	if !exists {
		return 0, false
	}
	intVar, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return intVar, true
}

func createPeerConnection(audioCodecs []webrtc.RTPCodecParameters, videoCodecs []webrtc.RTPCodecParameters, pcConfig *webrtc.Configuration) (*webrtc.PeerConnection, error) {
	m := &webrtc.MediaEngine{}
	for _, codec := range audioCodecs {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeAudio); err != nil {
			panic(err)
		}
	}
	for _, codec := range videoCodecs {
		if err := m.RegisterCodec(codec, webrtc.RTPCodecTypeVideo); err != nil {
			panic(err)
		}
	}
	i := &interceptor.Registry{}
	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		return nil, err
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m), webrtc.WithInterceptorRegistry(i))

	pc, err := api.NewPeerConnection(*pcConfig)
	return pc, err
}

func readRtpWriteTrack(port int, track *webrtc.TrackLocalStaticRTP) {
	var packetCounter uint16 = 0
	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
	if err != nil {
		panic(err)
	}
	defer func() {
		if err = listener.Close(); err != nil {
			panic(err)
		}
	}()
	inboundRTPPacket := make([]byte, 1600) // UDP MTU
	rtpPacket := &rtp.Packet{}
	for {
		n, _, err := listener.ReadFrom(inboundRTPPacket)
		if err != nil {
			panic(fmt.Sprintf("error during read: %s", err))
		}

		if err = rtpPacket.Unmarshal(inboundRTPPacket[:n]); err != nil {
			panic(err)
		}

		if rtpPacket.SequenceNumber != packetCounter+1 {
			fmt.Printf("unexpected sequence number on port %d - got: %d, expected: %d\n", port, rtpPacket.SequenceNumber, packetCounter+1)
		}
		packetCounter = rtpPacket.SequenceNumber

		if _, err = track.Write(inboundRTPPacket[:n]); err != nil {
			if errors.Is(err, io.ErrClosedPipe) {
				// The peerConnection has been closed.
				return
			}
			panic(err)
		}
	}
}

func readRtcpFromSender(sender *webrtc.RTPSender) {
	rtcpBuf := make([]byte, 1500)
	for {
		if _, _, rtcpErr := sender.Read(rtcpBuf); rtcpErr != nil {
			return
		}
	}
}

func main() {
	audioCodecs := []webrtc.RTPCodecParameters{}
	videoCodecs := []webrtc.RTPCodecParameters{}
	localRtpTracks := []*webrtc.TrackLocalStaticRTP{}

	whipEndpoint, whipEndpointExists := os.LookupEnv("WHIP_ENDPOINT")
	if !whipEndpointExists {
		log.Fatal("WHIP_ENDPOINT not set")
	}

	streamId := uuid.NewString()

	opusPort, opusPortExists := getPortFromEnv("OPUS_PORT")
	if opusPortExists {
		capability := webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeOpus,
			ClockRate:    48000,
			Channels:     2,
			SDPFmtpLine:  "minptime=10;useinbandfec=1",
			RTCPFeedback: nil,
		}
		audioCodecs = append(audioCodecs, webrtc.RTPCodecParameters{
			RTPCodecCapability: capability,
			PayloadType:        111,
		})
		opusTrack, err := webrtc.NewTrackLocalStaticRTP(
			capability,
			streamId+"-opus",
			streamId,
		)
		if err != nil {
			log.Fatal("Unexpected error creating opus track", err)
		}
		localRtpTracks = append(localRtpTracks, opusTrack)
		go readRtpWriteTrack(opusPort, opusTrack)
	}

	h264Port, h264PortExists := getPortFromEnv("H264_PORT")
	if h264PortExists {
		capability := webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			Channels:     0,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f",
			RTCPFeedback: nil,
		}
		videoCodecs = append(videoCodecs, webrtc.RTPCodecParameters{
			RTPCodecCapability: capability,
			PayloadType:        125,
		})
		h264Track, err := webrtc.NewTrackLocalStaticRTP(
			capability,
			streamId+"-h264",
			streamId,
		)
		if err != nil {
			log.Fatal("Unexpected error creating H.264 track", err)
		}
		localRtpTracks = append(localRtpTracks, h264Track)
		go readRtpWriteTrack(h264Port, h264Track)
	}
	pcConfig := webrtc.Configuration{}
	pc, err := createPeerConnection(audioCodecs, videoCodecs, &pcConfig)
	if err != nil {
		log.Fatal("Unexpected error creating the peer connection", err)
	}
	for _, t := range localRtpTracks {
		log.Printf("Adding Track (ID: %s, StreamID: %s)", t.ID(), t.StreamID())
		transceiver, err := pc.AddTransceiverFromTrack(t,
			webrtc.RtpTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			},
		)
		if err != nil {
			log.Fatal("Unexpected error adding RTP track", err)
		}
		go readRtcpFromSender(transceiver.Sender())
	}
	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateFailed {
			if closeErr := pc.Close(); closeErr != nil {
				log.Fatal("ICE Connection state is failed", err)
			}
		}
	})
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Fatal("Unexpected error creating offer", err)
	}
	err = pc.SetLocalDescription(offer)
	if err != nil {
		log.Fatal("PeerConnection could not set local offer.", err)
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	<-gatherComplete

	resp, err := http.Post(whipEndpoint, "application/sdp", strings.NewReader(offer.SDP))
	if err != nil {
		panic(err)
	}
	if err != nil {
		log.Fatal("Error making http request", err)
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Fatal("Problem reading HTTP response body", err)
	}
	answer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer,
		SDP:  string(respBody),
	}
	err = pc.SetRemoteDescription(answer)
	if err != nil {
		log.Fatal("PeerConnection could not set remote answer.", err)
	}

	sigs := make(chan os.Signal)
	done := make(chan bool, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		fmt.Println("Received " + sig.String() + " signal.")
		fmt.Println("Closing peer connection...")
		// to do: make DELETE request to WHIP server
		pc.Close()
		done <- true
	}()
	fmt.Println("Press 'Ctrl+C' to finish...")
	<-done
	fmt.Println("Exiting")
}
