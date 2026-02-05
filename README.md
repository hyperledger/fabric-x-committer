<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Fabric-X Committer

[![Coverage Status](https://coveralls.io/repos/github/hyperledger/fabric-x-committer/badge.svg?branch=main)](https://coveralls.io/github/hyperledger/fabric-x-committer?branch=main)

## Setup and Testing

See [setup](docs/setup.md) for details on prerequisites and quick start guide.

## Background
The lifecycle of a transaction consists of 3 main stages:
* **Execution**: First the transaction is sent to an **endorser** that will execute the transaction based on its current view of the ledger (this view may be stale). Then the endorser signs the transaction and forwards it to the next stage.
* **Ordering**: The **orderer** will receive in parallel the signed transactions from the endorsers and will send them in a specific order to the next stage.
* **Validation**: It takes place at the committer and it checks whether:
  * the signature is valid (not corrupt and it belongs to the endorsers)
  * the tokens (inputs or **Serial Numbers/SN**) have not already spent in a previous transaction (using the order as defined by the orderer)
