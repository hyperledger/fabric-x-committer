<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Prerequisites

### Golang 1.24.3

#### Mac:

```shell
brew install go@1.24.3
```

#### Ubuntu:

```shell
apt-get update
wget https://go.dev/dl/go1.24.3.linux-amd64.tar.gz
tar xvf ./go1.24.3.linux-amd64.tar.gz
rm ./go1.24.3.linux-amd64.tar.gz
mv ./go/ /usr/local/
```

Add the following lines in `~/.profile`:

```shell
export GOPATH=$HOME/go
export PATH=$PATH:/usr/local/go/bin:$GOPATH/bin
```

Test the version:

```shell
go version
```

### Docker client

#### Mac:

```shell
brew install docker
```

To enable network connectivity to Docker containers from macOS, run the following commands:

```shell
brew install chipmk/tap/docker-mac-net-connect
```

```shell
sudo brew services start chipmk/tap/docker-mac-net-connect
```

If you’re still experiencing issues, try restarting the service with the following command:
```shell
sudo brew services restart chipmk/tap/docker-mac-net-connect
```


#### Ubuntu:

Follow instructions [here](https://docs.docker.com/engine/install/ubuntu/)

To grant access to a non-root user:

```shell
apt install acl
setfacl --modify user:cbdcdemo:rw /var/run/docker.sock
```

# Quickstart

## Build

You can build locally or via a docker.

Local build:

```shell
make build
```

Docker build:

```shell
make build-docker
```

Build linux binaries for remote machines:

```shell
make build os=linux arch=amd64 output_dir=./bin-linux
```

## Test

To execute the tests, use:

```shell
make test
```

## Golang Development Dependencies Installation

```shell
scripts/install-dev-dependencies.sh
```

#### Note on YugabyteDB

In some tests, a DB is required (YugabyteDB or PostgreSQL).
The test harness will either create a YugabyteDB Docker instance or reuse an existing instance.
Therefore, the test harness will not shut down the DB container between tests.
Once you no longer intend to run tests, you can stop the container using `make kill-test-docker`.

It is possible to use your own YugabyteDB instance by setting the following environment variable: `DB_DEPLOYMENT=local`.
This will instruct the test harness to connect to your local DB instance at port 5433, rather than creating any Docker instance.
YugabyteDB's [quick start guide](https://docs.yugabyte.com/preview/quick-start/) offers more information on how to install and run the database locally.

Example: Start Yugabyte via docker

```bash
docker run --name sc_yugabyte_unit_tests \
  --platform linux/amd64 \
  -p 5433:5433 \
  -d yugabytedb/yugabyte:2.20.2.3-b2 \
  bin/yugabyted start \
  --background=false \
  --advertise_address=0.0.0.0 \
  --callhome=false \
  --fault_tolerance=none \
  --ui=false \
  --tserver_flags=ysql_max_connections=5000 \
  --insecure
```
