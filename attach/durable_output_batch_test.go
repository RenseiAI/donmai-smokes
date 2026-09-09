package attach_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/RenseiAI/donmai/attachclient"
	"github.com/RenseiAI/donmai/attachwire"
	attachwirev2 "github.com/RenseiAI/donmai/attachwire/v2"
	"github.com/coder/websocket"
)

// This hermetic consumer exercises the exported API over a real WebSocket.
// The relay receives the entire output window before issuing its delayed ACK;
// stop-and-wait cannot satisfy that contract. The retained suffix and following
// Marker must arrive byte-identically, without advancing across an unknown ACK.
func TestDurableOutputBatchExternalContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := func(seq uint64, payload []byte) []byte {
		return (attachwire.Frame{Type: attachwire.TypeOutput, Seq: seq, RelTime: 41, Payload: payload}).Encode()
	}
	raws := [][]byte{output(6, []byte{0, 255, '\r', '\n'}), output(7, []byte("seven")), output(8, []byte("eight")), output(9, []byte("nine"))}
	marker := (attachwire.Frame{Type: attachwire.TypeMarker, Seq: 10, RelTime: 42, Payload: []byte("finished")}).Encode()
	relayDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{attachwirev2.SubprotocolVersion}})
		if err != nil {
			relayDone <- err
			return
		}
		defer conn.CloseNow() //nolint:errcheck // fixture owns this connection
		if _, _, err := conn.Read(ctx); err != nil {
			relayDone <- err
			return
		}
		active, _ := attachwirev2.BuildControlFrame(attachwirev2.CarrierActive{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5})
		if err := conn.Write(ctx, websocket.MessageBinary, active.Encode()); err != nil {
			relayDone <- err
			return
		}
		expect := func(expected [][]byte) error {
			for _, want := range expected {
				kind, raw, err := conn.Read(ctx)
				if err != nil {
					return err
				}
				if kind != websocket.MessageBinary || !bytes.Equal(raw, want) {
					return fmt.Errorf("relay received changed or reordered source bytes")
				}
			}
			return nil
		}
		ack := func(seq attachwirev2.DecimalUint64) error {
			frame, _ := attachwirev2.BuildControlFrame(attachwirev2.HostAck{PTYEpoch: 3, CarrierEpoch: 9, AckSeq: seq})
			return conn.Write(ctx, websocket.MessageBinary, frame.Encode())
		}
		if err := expect(raws); err != nil {
			relayDone <- err
			return
		}
		timer := time.NewTimer(75 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			relayDone <- ctx.Err()
			return
		case <-timer.C:
		}
		if err := ack(6); err != nil {
			relayDone <- err
			return
		}
		if err := expect(raws[1:]); err != nil {
			relayDone <- err
			return
		}
		if err := ack(9); err != nil {
			relayDone <- err
			return
		}
		if err := expect([][]byte{marker}); err != nil {
			relayDone <- err
			return
		}
		relayDone <- ack(10)
		<-ctx.Done()
	}))
	defer server.Close()
	token := batchConsumerToken(t)
	candidate, err := attachclient.DialV2HostCandidate(ctx, attachclient.V2HostConfig{
		AttachURL:         strings.Replace(server.URL, "http://", "ws://", 1) + "/v2/rooms/batch-smoke",
		TokenSource:       func(context.Context) (string, error) { return token, nil },
		ResumeDisposition: &attachclient.V2ResumeDisposition{ProofSchemaVersion: attachclient.V2ProofSchemaV2, Authority: attachclient.V2ResumeSameHandoff, State: attachclient.V2ResumeActive, PTYEpoch: 3, CarrierEpoch: 9, AckSeq: 5},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close() //nolint:errcheck // smoke owns candidate
	if _, err := candidate.Activate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][][]byte{
		make([][]byte, attachclient.DurableOutputBatchMaxFrames+1),
		{output(6, make([]byte, attachclient.DurableOutputBatchMaxBytes))},
	} {
		if ack, err := candidate.SendRawFramesDurable(ctx, invalid); err == nil || ack != 5 {
			t.Fatalf("bounds refusal=%d,%v want unchanged5 with error", ack, err)
		}
	}
	short, stop := context.WithTimeout(ctx, 200*time.Millisecond)
	ack, err := candidate.SendRawFramesDurable(short, raws)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) || ack != 6 {
		t.Fatalf("partial durable window=%d,%v; want ACK6 and timeout", ack, err)
	}
	if _, err := candidate.SendRawFramesDurable(ctx, [][]byte{output(7, []byte("changed")), raws[2], raws[3]}); err == nil {
		t.Fatal("changed suffix accepted")
	}
	if err := candidate.SendRawFrameDurable(ctx, marker); err == nil {
		t.Fatal("Marker overtook unacknowledged output suffix")
	}
	if ack, err := candidate.SendRawFramesDurable(ctx, raws[1:]); err != nil || ack != 9 {
		t.Fatalf("exact suffix replay=%d,%v", ack, err)
	}
	if err := candidate.SendRawFrameDurable(ctx, marker); err != nil {
		t.Fatal(err)
	}
	if err := <-relayDone; err != nil {
		t.Fatal(err)
	}
	cancel()
	t.Log("external batch: bounded bytes, delayed contiguous ACK, exact suffix replay, ordered Marker")
}

func batchConsumerToken(t *testing.T) string {
	t.Helper()
	claims := map[string]any{
		"sessionId": "batch-smoke", "roomId": "batch-smoke", "role": "host", "epoch": 3, "carrier_epoch": 9,
		"handoff_nonce":               base64.RawURLEncoding.EncodeToString(make([]byte, 32)),
		"prepared_correlation_digest": strings.Repeat("a", 64), "proof_schema_version": "2",
		"store_authority_id": "smoke-store", "proof_revision": "1", "proof_digest": strings.Repeat("b", 64),
		"carrier_boundary": "4", "resolved_boundary": "4", "last_host_seq": "4",
		"reservation_request_id": "223e4567-e89b-42d3-a456-426614174000", "reservation_request_digest": strings.Repeat("c", 64),
		"reserved_candidate_carrier_epoch": "9", "carrier_epoch_floor": "9", "predecessor_abandonment": nil,
		"protocol": attachwirev2.ProtocolVersion, "orgId": "smoke-org", "iat": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(), "aud": "relay", "jti": "123e4567-e89b-42d3-a456-426614174000",
	}
	header, err := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(make([]byte, 64))
}
