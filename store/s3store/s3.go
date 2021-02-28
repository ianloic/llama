// Copyright 2020 Nelson Elhage
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package s3store

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/url"
	"path"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/request"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/nelhage/llama/store"
	"github.com/nelhage/llama/store/internal/storeutil"
	"github.com/nelhage/llama/tracing"
	"golang.org/x/sync/errgroup"
)

type Options struct {
	DisableHeadCheck bool
}

type Store struct {
	opts    Options
	session *session.Session
	s3      *s3.S3
	url     *url.URL

	cache storeutil.Cache
}

func FromSession(s *session.Session, address string) (*Store, error) {
	return FromSessionAndOptions(s, address, Options{})
}

func FromSessionAndOptions(s *session.Session, address string, opts Options) (*Store, error) {
	u, e := url.Parse(address)
	if e != nil {
		return nil, fmt.Errorf("Parsing store: %q: %w", address, e)
	}
	if u.Scheme != "s3" {
		return nil, fmt.Errorf("Object store: %q: unsupported scheme %s", address, u.Scheme)
	}
	svc := s3.New(s, aws.NewConfig().WithS3DisableContentMD5Validation(true))
	svc.Handlers.Sign.PushFront(func(r *request.Request) {
		r.HTTPRequest.Header.Add("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
	})
	return &Store{
		opts:    opts,
		session: s,
		s3:      svc,
		url:     u,
	}, nil
}

func (s *Store) Store(ctx context.Context, obj []byte) (string, error) {
	ctx, span := tracing.StartSpan(ctx, "s3.store")
	defer span.End()
	id := storeutil.HashObject(obj)

	if s.cache.HasObject(id) {
		return id, nil
	}

	key := aws.String(path.Join(s.url.Path, id))
	var err error

	span.AddField("object_id", id)

	upload := s.cache.StartUpload(id)
	defer upload.Rollback()

	if !s.opts.DisableHeadCheck {
		_, err = s.s3.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
			Bucket: &s.url.Host,
			Key:    key,
		})
		if err == nil {
			upload.Complete()
			span.AddField("s3.exists", true)
			return id, nil
		}
		if reqerr, ok := err.(awserr.RequestFailure); ok && reqerr.StatusCode() == 404 {
			// 404 not found -- do the upload
		} else {
			return "", err
		}
	}

	span.AddField("s3.write_bytes", len(obj))

	_, err = s.s3.PutObjectWithContext(ctx, &s3.PutObjectInput{
		Body:   bytes.NewReader(obj),
		Bucket: &s.url.Host,
		Key:    key,
	})
	if err != nil {
		return "", err
	}
	upload.Complete()
	return id, nil
}

const getConcurrency = 32

func (s *Store) getOne(ctx context.Context, id string) ([]byte, error) {
	ctx, span := tracing.StartSpan(ctx, "s3.get_one")
	defer span.End()

	resp, err := s.s3.GetObjectWithContext(ctx, &s3.GetObjectInput{
		Bucket: &s.url.Host,
		Key:    aws.String(path.Join(s.url.Path, id)),
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	gotId := storeutil.HashObject(body)
	if gotId != id {
		return nil, fmt.Errorf("object store mismatch: got csum=%s expected %s", gotId, id)
	}
	u := s.cache.StartUpload(id)
	u.Complete()

	span.AddField("s3.read_bytes", len(body))
	return body, nil
}

func (s *Store) GetObjects(ctx context.Context, gets []store.GetRequest) {
	ctx, span := tracing.StartSpan(ctx, "s3.get_objects")
	defer span.End()
	grp, ctx := errgroup.WithContext(ctx)
	jobs := make(chan int)

	grp.Go(func() error {
		defer close(jobs)
		for i := range gets {
			jobs <- i
		}
		return nil
	})
	for i := 0; i < getConcurrency; i++ {
		grp.Go(func() error {
			for idx := range jobs {
				gets[idx].Data, gets[idx].Err = s.getOne(ctx, gets[idx].Id)
			}
			return nil
		})
	}

	if err := grp.Wait(); err != nil {
		log.Fatalf("GetObjects: internal error %s", err)
	}
}
