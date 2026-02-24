<!--
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
-->
# Postgres Support

You can use VCService with a local Postgres instance as an alternative to Yugabyte.  

Run VCService with the following environment variables set and make sure your Postgres instance is running.

```bash
export DB_DEPLOYMENT=local
```

## Start Postgres
Note that VCService uses `yugabyte:yugabye` as default user and password, and runs on port `5344`. 
Please make sure you set up your Postgres instance accordingly.

For local testing with Postgres in docker you can simply use the following snipped.
```bash
docker run --name sc_postgres_unit_tests \
  -e POSTGRES_PASSWORD=yugabyte \
  -e POSTGRES_USER=yugabyte \
  -p 5433:5432 \
  -d postgres:16.9-alpine3.21
```

You can kill the instance by running:
```bash
docker ps -aq -f name=sc_postgres_unit_tests | xargs docker rm -f
```

## Testing

Once Postgres is up and running you can run the tests of VC service.
```bash
DB_DEPLOYMENT=local go test ./service/vc/dbtest/...
```

