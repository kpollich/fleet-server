// Copyright Elasticsearch B.V. and/or licensed to Elasticsearch B.V. under one
// or more contributor license agreements. Licensed under the Elastic License;
// you may not use this file except in compliance with the Elastic License.

package bulk

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/mailru/easyjson"
	"github.com/rs/zerolog"
	"go.elastic.co/apm/v2"
)

const (
	rPrefix = "{\"docs\": ["
	rSuffix = "]}"
)

func (b *Bulker) ReadRaw(ctx context.Context, index, id string, opts ...Opt) (*MgetResponseItem, error) {
	span, ctx := apm.StartSpan(ctx, "Bulker: readRaw", "bulker")
	defer span.End()
	opt := b.parseOpts(append(opts, withAPMLinkedContext(ctx))...)
	blk := b.newBlk(ActionRead, opt)

	// Serialize request
	const kSlop = 64
	blk.buf.Grow(kSlop)

	if err := b.writeMget(&blk.buf, index, id); err != nil {
		return nil, err
	}

	// Process response
	resp := b.dispatch(ctx, blk)
	if resp.err != nil {
		return nil, resp.err
	}
	b.freeBlk(blk)

	// TODO if we can ever set span links after creation we can inject the flushQueue span into resp and link to the action span here.

	// Interpret response, looking for generated id
	r, ok := resp.data.(*MgetResponseItem)
	if !ok {
		return nil, fmt.Errorf("unable to cast response to *MgetResponseItem, detected type: %T", resp.data)
	}
	return r, nil
}

func (b *Bulker) Read(ctx context.Context, index, id string, opts ...Opt) ([]byte, error) {
	span, ctx := apm.StartSpan(ctx, "Bulker: read", "bulker")
	defer span.End()
	r, err := b.ReadRaw(ctx, index, id, opts...)
	if err != nil {
		return nil, err
	}

	return r.Source, nil
}

func (b *Bulker) flushRead(ctx context.Context, queue queueT) error {
	start := time.Now()

	const kRoughEstimatePerItem = 256

	bufSz := queue.cnt * kRoughEstimatePerItem
	if bufSz < queue.pending+len(rSuffix) {
		bufSz = queue.pending + len(rSuffix)
	}

	buf := bytes.NewBufferString(rPrefix)
	buf.Grow(bufSz)

	// Each item a JSON array element followed by comma
	queueCnt := 0
	links := []apm.SpanLink{}
	for n := queue.head; n != nil; n = n.next {
		buf.Write(n.buf.Bytes())
		queueCnt += 1
		if n.spanLink != nil {
			links = append(links, *n.spanLink)
		}
	}
	if len(links) == 0 {
		links = nil
	}
	span, ctx := apm.StartSpanOptions(ctx, "Flush: read", "read", apm.SpanOptions{
		Links: links,
	})
	defer span.End()

	// Need to strip the last element and append the suffix
	payload := buf.Bytes()
	payload = append(payload[:len(payload)-1], []byte(rSuffix)...)

	// Do actual bulk request; and send response on chan
	req := esapi.MgetRequest{
		Body: bytes.NewReader(payload),
	}

	var refresh bool
	if queue.ty == kQueueRefreshRead {
		refresh = true
		req.Refresh = &refresh
	}

	res, err := req.Do(ctx, b.es)

	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("mod", kModBulk).Msg("bulker.flushRead: Error sending mget request to Elasticsearch")
		return err
	}

	if res.Body != nil {
		defer res.Body.Close()
	}

	if res.IsError() {
		zerolog.Ctx(ctx).Warn().Str("mod", kModBulk).Str("error.message", res.String()).Msg("bulker.flushRead: Error in mget request result to Elasticsearch")
		return parseError(res, zerolog.Ctx(ctx))
	}

	// Reuse buffer
	buf.Reset()

	bodySz, err := buf.ReadFrom(res.Body)
	if err != nil {
		zerolog.Ctx(ctx).Error().Err(err).Str("mod", kModBulk).Msg("Response error")
	}

	// prealloc slice
	var blk MgetResponse
	blk.Items = make([]MgetResponseItem, 0, queueCnt)

	if err = easyjson.Unmarshal(buf.Bytes(), &blk); err != nil {
		zerolog.Ctx(ctx).Error().Err(err).Str("mod", kModBulk).Msg("Unmarshal error")
		return err
	}

	zerolog.Ctx(ctx).Trace().
		Err(err).
		Bool("refresh", refresh).
		Str("mod", kModBulk).
		Dur("rtt", time.Since(start)).
		Int("cnt", len(blk.Items)).
		Int("bufSz", bufSz).
		Int64("bodySz", bodySz).
		Msg("flushRead")

	if len(blk.Items) != queueCnt {
		return fmt.Errorf("queue length mismatch for Mget")
	}

	// WARNING: Once we start pushing items to
	// the queue, the node pointers are invalid.
	// Do NOT return a non-nil value or failQueue
	// up the stack will fail.

	n := queue.head
	for i := range blk.Items {
		next := n.next // 'n' is invalid immediately on channel send
		item := &blk.Items[i]
		select {
		case n.ch <- respT{
			err:  item.deriveError(),
			idx:  n.idx,
			data: item,
		}:
		default:
			panic("Unexpected blocked response channel on flushRead")
		}
		n = next
	}

	return nil
}
