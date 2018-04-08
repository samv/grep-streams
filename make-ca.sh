#!/bin/sh
set -xe
openssl req -x509 -new -nodes -key myCA.key -sha256 -days 1825 -out myCA.pem
openssl genrsa -out localhost.key 2048
openssl req -new -key localhost.key -out localhost.csr
openssl x509 -req -in localhost.csr -CA myCA.pem -CAkey myCA.key -CAcreateserial -out localhost.crt -days 1825 -sha256
