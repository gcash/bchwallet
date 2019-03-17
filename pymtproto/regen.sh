#!/bin/sh

protoc -I. paymentrequest.proto --go_out=./payments