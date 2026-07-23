// Copyright IBM Corp. All Rights Reserved.
//
// SPDX-License-Identifier: Apache-2.0
//
module github.com/hyperledger/fabric-x-committer

go 1.26.5

require (
	github.com/cenkalti/backoff/v5 v5.0.3
	github.com/cockroachdb/errors v1.14.0
	github.com/consensys/gnark-crypto v0.20.1
	github.com/fsouza/go-dockerclient v1.13.2
	github.com/gavv/httpexpect/v2 v2.17.0
	github.com/go-playground/validator/v10 v10.30.3
	github.com/go-task/slim-sprig/v3 v3.0.0
	github.com/go-viper/mapstructure/v2 v2.5.0
	github.com/golang/protobuf v1.5.4
	github.com/google/uuid v1.6.0
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.29.0
	github.com/hyperledger/fabric-lib-go v1.1.5-0.20260708100132-163bcc919208
	github.com/hyperledger/fabric-protos-go-apiv2 v0.3.7
	github.com/hyperledger/fabric-x-common v0.2.9-0.20260722080022-42995bb28ab4
	github.com/jackc/pgerrcode v0.0.0-20250907135507-afb5586c32a6
	github.com/jackc/puddle/v2 v2.2.2
	github.com/moby/moby/api v1.55.0
	github.com/moby/moby/client v0.5.0
	github.com/prometheus/client_golang v1.23.2
	github.com/prometheus/client_model v0.6.2
	github.com/spf13/cobra v1.10.2
	github.com/spf13/viper v1.21.0
	github.com/stretchr/testify v1.11.1
	github.com/yugabyte/pgx/v5 v5.7.6-yb-1
	go.uber.org/mock v0.6.0
	go.uber.org/zap v1.28.0
	golang.org/x/exp v0.0.0-20260709172345-9ea1abe57597
	golang.org/x/sync v0.22.0
	golang.org/x/text v0.40.0
	golang.org/x/time v0.15.0
	google.golang.org/genproto/googleapis/api v0.0.0-20260706201446-f0a921348800
	google.golang.org/grpc v1.82.0
	google.golang.org/protobuf v1.36.11
)

tool (
	github.com/Kunde21/markdownfmt/v3/cmd/markdownfmt
	github.com/googleapis/api-linter/v2/cmd/api-linter
	github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway
	go.uber.org/mock/mockgen
	golang.org/x/perf/cmd/benchstat
	golang.org/x/tools/cmd/goimports
	google.golang.org/grpc/cmd/protoc-gen-go-grpc
	google.golang.org/protobuf/cmd/protoc-gen-go
	gotest.tools/gotestsum
	mvdan.cc/gofumpt
)

require (
	bitbucket.org/creachadair/stringset v0.0.14 // indirect
	cloud.google.com/go/longrunning v1.0.0 // indirect
	github.com/Azure/go-ansiterm v0.0.0-20250102033503-faa5f7b0171c // indirect
	github.com/IBM/idemix v0.2.0 // indirect
	github.com/IBM/mathlib v0.3.0 // indirect
	github.com/Kunde21/markdownfmt/v3 v3.1.0 // indirect
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/TylerBrock/colorjson v0.0.0-20200706003622-8a50f05110d2 // indirect
	github.com/aclements/go-moremath v0.0.0-20210112150236-f10218a38794 // indirect
	github.com/ajg/form v1.7.1 // indirect
	github.com/andybalholm/brotli v1.2.1 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/bitfield/gotestdox v0.2.2 // indirect
	github.com/bits-and-blooms/bitset v1.24.4 // indirect
	github.com/bmatcuk/doublestar/v4 v4.10.0 // indirect
	github.com/bufbuild/protocompile v0.14.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.7.0 // indirect
	github.com/cockroachdb/logtags v0.0.0-20241215232642-bb51bb14a506 // indirect
	github.com/cockroachdb/redact v1.1.8 // indirect
	github.com/containerd/errdefs v1.0.0 // indirect
	github.com/containerd/errdefs/pkg v0.3.0 // indirect
	github.com/containerd/log v0.1.0 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/distribution/reference v0.6.0 // indirect
	github.com/dnephin/pflag v1.0.7 // indirect
	github.com/docker/go-connections v0.7.0 // indirect
	github.com/docker/go-units v0.5.0 // indirect
	github.com/expr-lang/expr v1.17.8 // indirect
	github.com/fatih/color v1.19.0 // indirect
	github.com/fatih/structs v1.1.0 // indirect
	github.com/felixge/httpsnoop v1.0.4 // indirect
	github.com/fsnotify/fsnotify v1.10.1 // indirect
	github.com/gabriel-vasile/mimetype v1.4.13 // indirect
	github.com/gertd/go-pluralize v0.2.1 // indirect
	github.com/getsentry/sentry-go v0.46.2 // indirect
	github.com/go-logfmt/logfmt v0.6.1 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/gobwas/glob v0.2.3 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/snappy v1.0.0 // indirect
	github.com/google/go-querystring v1.2.0 // indirect
	github.com/google/shlex v0.0.0-20191202100458-e7afc7fbc510 // indirect
	github.com/googleapis/api-linter/v2 v2.1.0 // indirect
	github.com/gorilla/websocket v1.5.3 // indirect
	github.com/hyperledger-labs/SmartBFT v1.0.1 // indirect
	github.com/hyperledger/fabric-amcl v0.0.0-20230602173724-9e02669dceb2 // indirect
	github.com/imkira/go-interpol v1.1.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/pgx/v5 v5.9.2 // indirect
	github.com/klauspost/compress v1.18.6 // indirect
	github.com/kr/pretty v0.3.1 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/mattn/go-runewidth v0.0.23 // indirect
	github.com/miekg/pkcs11 v1.1.2 // indirect
	github.com/mitchellh/go-wordwrap v1.0.1 // indirect
	github.com/moby/docker-image-spec v1.3.1 // indirect
	github.com/moby/go-archive v0.2.0 // indirect
	github.com/moby/patternmatcher v0.6.1 // indirect
	github.com/moby/sys/sequential v0.6.0 // indirect
	github.com/moby/sys/user v0.4.0 // indirect
	github.com/moby/sys/userns v0.1.0 // indirect
	github.com/moby/term v0.5.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/nxadm/tail v1.4.11 // indirect
	github.com/olekukonko/tablewriter v0.0.5 // indirect
	github.com/opencontainers/go-digest v1.0.0 // indirect
	github.com/opencontainers/image-spec v1.1.1 // indirect
	github.com/pelletier/go-toml/v2 v2.3.1 // indirect
	github.com/pkg/diff v0.0.0-20241224192749-4e6772a4315c // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	github.com/prometheus/common v0.67.5 // indirect
	github.com/prometheus/procfs v0.20.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	github.com/sagikazarmark/locafero v0.12.0 // indirect
	github.com/sanity-io/litter v1.5.8 // indirect
	github.com/sergi/go-diff v1.4.0 // indirect
	github.com/sirupsen/logrus v1.9.4 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/pflag v1.0.10 // indirect
	github.com/stoewer/go-strcase v1.3.1 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/syndtr/goleveldb v1.0.1-0.20220721030215-126854af5e6d // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasthttp v1.71.0 // indirect
	github.com/xeipuuv/gojsonpointer v0.0.0-20190905194746-02993c407bfb // indirect
	github.com/xeipuuv/gojsonreference v0.0.0-20180127040603-bd5ef7bd5415 // indirect
	github.com/xeipuuv/gojsonschema v1.2.0 // indirect
	github.com/yalp/jsonpath v0.0.0-20180802001716-5cc68e5049a0 // indirect
	github.com/yudai/gojsondiff v1.0.0 // indirect
	github.com/yudai/golcs v0.0.0-20170316035057-ecda9a501e82 // indirect
	github.com/yuin/goldmark v1.8.2 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.68.0 // indirect
	go.opentelemetry.io/otel v1.43.0 // indirect
	go.opentelemetry.io/otel/metric v1.43.0 // indirect
	go.opentelemetry.io/otel/trace v1.43.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.yaml.in/yaml/v2 v2.4.4 // indirect
	go.yaml.in/yaml/v3 v3.0.4 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/perf v0.0.0-20260709024250-82a0b07e230d // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/term v0.45.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260630182238-925bb5da69e7 // indirect
	google.golang.org/grpc/cmd/protoc-gen-go-grpc v1.6.2 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	gotest.tools/gotestsum v1.13.0 // indirect
	moul.io/http2curl/v2 v2.3.0 // indirect
	mvdan.cc/gofumpt v0.10.0 // indirect
)
