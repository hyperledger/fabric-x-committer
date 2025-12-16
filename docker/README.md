<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Docker Registry 

## Artifactory

To simplify our SC prototype development, we use artifactory as our IBM internal docker registry.
The repo is available at https://eu.artifactory.swg-devops.com/artifactory/res-decentralized-trust-team-sc-docker-local/ and requires permission to use it.

### Setting up your account

First, we need to add you to the permission group.
Please contact @marcus via IBM Slack or [bur@zurich.ibm.com](bur@zurich.ibm.com).

Next, use your w3id to login at https://eu.artifactory.swg-devops.com/artifactory/res-decentralized-trust-team-sc-docker-local/.
On the top-right, click on your w3id and open ["Edit profile"](https://eu.artifactory.swg-devops.com/ui/admin/artifactory/user_profile).
Click on "Generate an Identity Token", give it a name and keep the token string somewhere at a save place. :P
You will need the token string later, to login with docker.


### Docker Login

```bash
docker login docker-eu.artifactory.swg-devops.com
// please use your w3id as user and the token as pasword
```

### Work with the Docker Registry

In this example, we illustrate how to pull and push the `sc_runner` docker image from our artifactory.

#### Pull
```bash
docker pull docker-eu.artifactory.swg-devops.com/res-decentralized-trust-team-sc-docker-local/sc_runner:latest
docker tag docker-eu.artifactory.swg-devops.com/res-decentralized-trust-team-sc-docker-local/sc_runner:latest sc_runner:latest
```

#### Push
```bash
docker tag sc_runner:latest docker-eu.artifactory.swg-devops.com/res-decentralized-trust-team-sc-docker-local/sc_runner:latest
docker push docker-eu.artifactory.swg-devops.com/res-decentralized-trust-team-sc-docker-local/sc_runner:latest
```

## Run Image
The image contains the binaries, default config files, and default genesis block.
By default, the test node image will run a full system: DB, mock-orderer, committer, and a simple generated load with mutual tls mode.
```shell
docker run -it --rm icr.io/cbdc/committer-test-node:0.0.2
```

To control which parts of the system will run, you can use the `run` script as follows:
```shell
docker run -it --rm icr.io/cbdc/committer-test-node:0.0.2 run [db] [orderer] [committer] [loadgen]
```
And add the ```--insecure``` flag for an insecure run.

For example, to run it with an existing orderer node:
```shell
docker run -it --rm \
    -v my-crypto/:/root/config/crypto/ \
    -e SC_SIDECAR_ORDERER_IDENTITY_MSP_DIR="/root/config/crypto/peerOrganizations/defaultpeerorg.example.com/peers/peerservice-machine1.defaultpeerorg.example.com/msp" \
    -e SC_SIDECAR_ORDERER_IDENTITY_ROOT_CA_PATHS="/root/config/crypto/ca-certs.pem" \
    -e SC_SIDECAR_ORDERER_CHANNEL_ID="real-channel" \
    -e SC_SIDECAR_ORDERER_ENDPOINT="real-orderer.com:7050" \
    icr.io/cbdc/committer-test-node:0.0.2 run db committer
```
This mounts the MSP folder and root CA, and override teh config to use a real orderer address.
