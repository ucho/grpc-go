/*
 *
 * Copyright 2014, Google Inc.
 * All rights reserved.
 *
 * Redistribution and use in source and binary forms, with or without
 * modification, are permitted provided that the following conditions are
 * met:
 *
 *     * Redistributions of source code must retain the above copyright
 * notice, this list of conditions and the following disclaimer.
 *     * Redistributions in binary form must reproduce the above
 * copyright notice, this list of conditions and the following disclaimer
 * in the documentation and/or other materials provided with the
 * distribution.
 *     * Neither the name of Google Inc. nor the names of its
 * contributors may be used to endorse or promote products derived from
 * this software without specific prior written permission.
 *
 * THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
 * "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
 * LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
 * A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
 * OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
 * SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
 * LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
 * DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
 * THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
 * (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
 * OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
 *
 */

package grpc

import (
	"io"
	"net"

	"github.com/golang/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/transport"
)

// recv receives and parses an RPC response.
// On error, it returns the error and indicates whether the call should be retried.
//
// TODO(zhaoq): Check whether the received message sequence is valid.
func recv(t transport.ClientTransport, c *callInfo, stream *transport.Stream, reply proto.Message) error {
	// Try to acquire header metadata from the server if there is any.
	var err error
	c.headerMD, err = stream.Header()
	if err != nil {
		return err
	}
	p := &parser{s: stream}
	for {
		if err = recvProto(p, reply); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	c.trailerMD = stream.Trailer()
	return nil
}

// sendRPC writes out various information of an RPC such as Context and Message.
func sendRPC(ctx context.Context, callHdr *transport.CallHdr, t transport.ClientTransport, args proto.Message, opts *transport.Options) (_ *transport.Stream, err error) {
	stream, err := t.NewStream(ctx, callHdr)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			if _, ok := err.(transport.ConnectionError); !ok {
				t.CloseStream(stream, err)
			}
		}
	}()
	// TODO(zhaoq): Support compression.
	outBuf, err := encode(args, compressionNone)
	if err != nil {
		return nil, transport.StreamErrorf(codes.Internal, "grpc: %v", err)
	}
	err = t.Write(stream, outBuf, opts)
	if err != nil {
		return nil, err
	}
	// Sent successfully.
	return stream, nil
}

// callInfo contains all related configuration and information about an RPC.
type callInfo struct {
	failFast  bool
	headerMD  metadata.MD
	trailerMD metadata.MD
}

// Invoke is called by the generated code. It sends the RPC request on the
// wire and returns after response is received.
func Invoke(ctx context.Context, method string, args, reply proto.Message, cc *ClientConn, opts ...CallOption) error {
	var c callInfo
	for _, o := range opts {
		if err := o.before(&c); err != nil {
			return toRPCErr(err)
		}
	}
	defer func() {
		for _, o := range opts {
			o.after(&c)
		}
	}()
	host, _, err := net.SplitHostPort(cc.target)
	if err != nil {
		return toRPCErr(err)
	}
	callHdr := &transport.CallHdr{
		Host:   host,
		Method: method,
	}
	topts := &transport.Options{
		Last:  true,
		Delay: false,
	}
	ts := 0
	var lastErr error // record the error that happened
	for {
		var (
			err    error
			t      transport.ClientTransport
			stream *transport.Stream
		)
		// TODO(zhaoq): Need a formal spec of retry strategy for non-failfast rpcs.
		if lastErr != nil && c.failFast {
			return toRPCErr(lastErr)
		}
		t, ts, err = cc.wait(ctx, ts)
		if err != nil {
			if lastErr != nil {
				// This was a retry; return the error from the last attempt.
				return toRPCErr(lastErr)
			}
			return Errorf(codes.Internal, "%v", err)
		}
		stream, err = sendRPC(ctx, callHdr, t, args, topts)
		if err != nil {
			if _, ok := err.(transport.ConnectionError); ok {
				lastErr = err
				continue
			}
			if lastErr != nil {
				return toRPCErr(lastErr)
			}
			return toRPCErr(err)
		}
		// Receive the response
		lastErr = recv(t, &c, stream, reply)
		if _, ok := lastErr.(transport.ConnectionError); ok {
			continue
		}
		t.CloseStream(stream, lastErr)
		if lastErr != nil {
			return toRPCErr(lastErr)
		}
		return Errorf(stream.StatusCode(), stream.StatusDesc())
	}
}
