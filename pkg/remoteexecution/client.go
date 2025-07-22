package remoteexecution

import (
	"context"
	"crypto/ecdh"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"iter"

	remoteexecution_pb "bonanza.build/pkg/proto/remoteexecution"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/secure-io/siv-go"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Client for issuing remote execution requests against a scheduler.
type Client[
	TAction proto.Message,
	TEvent any,
	TEventPtr interface {
		*TEvent
		proto.Message
	},
	TResult proto.Message,
] struct {
	executionClient        remoteexecution_pb.ExecutionClient
	clientPrivateKey       *ecdh.PrivateKey
	clientCertificateChain [][]byte
}

// NewClient creates a new remote execution client that uses a provided
// gRPC client and X.509 client key pair.
func NewClient[
	TAction proto.Message,
	TEvent any,
	TResult proto.Message,
	TEventPtr interface {
		*TEvent
		proto.Message
	},
](
	executionClient remoteexecution_pb.ExecutionClient,
	clientPrivateKey *ecdh.PrivateKey,
	clientCertificateChain [][]byte,
) *Client[TAction, TEvent, TEventPtr, TResult] {
	return &Client[TAction, TEvent, TEventPtr, TResult]{
		executionClient:        executionClient,
		clientPrivateKey:       clientPrivateKey,
		clientCertificateChain: clientCertificateChain,
	}
}

// RunAction encrypts an action and sends it to a scheduler to request
// its execution on a worker. An iterator is returned that yields any
// execution events reported by the worker. Upon completion, the result
// message is set.
func (c *Client[TAction, TEvent, TEventPtr, TResult]) RunAction(ctx context.Context, platformECDHPublicKey *ecdh.PublicKey, action TAction, actionAdditionalData *remoteexecution_pb.Action_AdditionalData, result TResult, errOut *error) iter.Seq[TEventPtr] {
	marshaledPlatformECDHPublicKey, err := x509.MarshalPKIXPublicKey(platformECDHPublicKey)
	if err != nil {
		*errOut = util.StatusWrapfWithCode(err, codes.InvalidArgument, "Failed to obtain marshal platform ECDH public key")
		return func(func(TEventPtr) bool) {}
	}

	// Compute shared secret for encrypting the action.
	sharedSecret, err := c.clientPrivateKey.ECDH(platformECDHPublicKey)
	if err != nil {
		*errOut = util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to obtain shared secret")
		return func(func(TEventPtr) bool) {}
	}

	actionKey := append([]byte(nil), sharedSecret...)
	actionKey[0] ^= 1
	actionAEAD, err := siv.NewGCM(actionKey)
	if err != nil {
		*errOut = util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to create AES-GCM-SIV for action")
		return func(func(TEventPtr) bool) {}
	}
	actionNonce := make([]byte, actionAEAD.NonceSize())

	// Marshal and encrypt the action. We wrap the action in a
	// google.protobuf.Any, so that the worker can reliably reject
	// the action if it's not the right type for that kind of
	// worker.
	actionAny, err := anypb.New(action)
	if err != nil {
		*errOut = util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to embed action into Any message")
		return func(func(TEventPtr) bool) {}
	}
	actionData, err := proto.Marshal(actionAny)
	if err != nil {
		*errOut = util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to marshal action")
		return func(func(TEventPtr) bool) {}
	}
	marshaledActionAdditionalData, err := proto.Marshal(actionAdditionalData)
	if err != nil {
		*errOut = util.StatusWrapWithCode(err, codes.InvalidArgument, "Failed to marshal action additional data")
		return func(func(TEventPtr) bool) {}
	}
	actionCiphertext := actionAEAD.Seal(nil, actionNonce, actionData, marshaledActionAdditionalData)

	return func(yield func(TEventPtr) bool) {
		ctxWithCancel, cancel := context.WithCancel(ctx)
		defer cancel()
		client, err := c.executionClient.Execute(ctxWithCancel, &remoteexecution_pb.ExecuteRequest{
			Action: &remoteexecution_pb.Action{
				PlatformPkixPublicKey:  marshaledPlatformECDHPublicKey,
				ClientCertificateChain: c.clientCertificateChain,
				Nonce:                  actionNonce,
				AdditionalData:         actionAdditionalData,
				Ciphertext:             actionCiphertext,
			},
			// TODO: Priority.
		})
		if err != nil {
			*errOut = err
			return
		}
		defer func() {
			cancel()
			for {
				if _, err := client.Recv(); err != nil {
					return
				}
			}
		}()

		executionEventAdditionalData := sha256.Sum256(actionCiphertext)
		for {
			response, err := client.Recv()
			if err != nil {
				*errOut = err
				return
			}

			switch stage := response.Stage.(type) {
			case *remoteexecution_pb.ExecuteResponse_Executing_:
				// Worker has posted an execution event.
				// Unmarshal it and yield it to the
				// caller.
				if lastEventMessage := stage.Executing.LastEvent; lastEventMessage != nil {
					lastEventKey := append([]byte(nil), sharedSecret...)
					lastEventKey[0] ^= 2
					completionEventAEAD, err := siv.NewGCM(lastEventKey)
					if err != nil {
						*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to create AES-GCM-SIV for last event")
						return
					}

					lastEventData, err := completionEventAEAD.Open(
						/* dst = */ nil,
						lastEventMessage.Nonce,
						lastEventMessage.Ciphertext,
						executionEventAdditionalData[:],
					)
					if err != nil {
						*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to decrypt last event")
						return
					}

					var lastEvent TEvent
					if err := proto.Unmarshal(lastEventData, TEventPtr(&lastEvent)); err != nil {
						*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to unmarshal last event")
						return
					}

					if !yield(&lastEvent) {
						*errOut = nil
						return
					}
				}
			case *remoteexecution_pb.ExecuteResponse_Completed_:
				// Action has completed. Unmarshal and
				// return the completion event.
				completionEventMessage := stage.Completed.CompletionEvent
				if completionEventMessage == nil {
					*errOut = status.Error(codes.Internal, "Action completed, but no completion event was returned")
					return
				}

				completionEventKey := append([]byte(nil), sharedSecret...)
				completionEventKey[0] ^= 3
				completionEventAEAD, err := siv.NewGCM(completionEventKey)
				if err != nil {
					*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to create AES-GCM-SIV for completion event")
					return
				}

				completionEventData, err := completionEventAEAD.Open(
					/* dst = */ nil,
					completionEventMessage.Nonce,
					completionEventMessage.Ciphertext,
					executionEventAdditionalData[:],
				)
				if err != nil {
					*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to decrypt completion event")
					return
				}

				if err := proto.Unmarshal(completionEventData, result); err != nil {
					*errOut = util.StatusWrapWithCode(err, codes.Internal, "Failed to unmarshal completion event")
					return
				}

				*errOut = nil
				return
			}
		}
	}
}

// ParseCertificateChain parses an X.509 certificate chain, so that it
// can be provided to NewClient().
func ParseCertificateChain(data []byte) ([][]byte, error) {
	var clientCertificates [][]byte
	for certificateBlock, remainder := pem.Decode(data); certificateBlock != nil; certificateBlock, remainder = pem.Decode(remainder) {
		if certificateBlock.Type != "CERTIFICATE" {
			return nil, status.Errorf(codes.InvalidArgument, "Client certificate PEM block at index %d is not of type CERTIFICATE", len(clientCertificates))
		}
		clientCertificates = append(clientCertificates, certificateBlock.Bytes)
	}
	return clientCertificates, nil
}
