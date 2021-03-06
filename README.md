Skia-Buildbot Repository
========================

This repo contains infrastructure code for Skia.


Getting the Source Code
=======================

The main source code repository is a Git repository hosted at
https://skia.googlesource.com/buildbot. Although it is possible to check out
this repository directly with git clone or using gclient fetch, it is preferred to use go get so
that the code is arranged correctly for Go. If this is your first time working on Go code, read
about [the GOPATH environment variable](https://golang.org/doc/code.html#GOPATH). Make sure that
$GOPATH/bin comes before /usr/bin in your PATH. If you have GOPATH set, run:

```
$ go get -u -t go.skia.org/infra/...
```

This fetches the repository into your $GOPATH directory along with all the
Go dependencies.
Note: go.skia.org is a custom import path and will only work if used like the examples
[here](http://golang.org/cmd/go/#hdr-Remote_import_paths).

Install [Node.js](https://nodejs.org/en/download/) (not as root) and add the bin dir to your
path. Optionally run `npm install npm -g`, as suggested by the
[npm getting started doc](https://docs.npmjs.com/getting-started/installing-node#updating-npm).

Install other dependencies:

```
$ sudo apt-get install python-django
$ go get github.com/kisielk/errcheck \
  golang.org/x/tools/cmd/goimports \
  github.com/luci/luci-go/client/cmd/isolate
$ npm install -g polylint bower
```

Build from GOPATH:

```
$ cd $GOPATH/src/go.skia.org/infra/
$ make all
```

Database Setup for Testing
==========================

Tests which use the database package's testutils require you to have a MySQL instance running with a
database named "sk_testing" and users called "readwrite" and "test_root" with appropriate
permissions for sk_testing. The 'setup_test_db' script in 'go/database' is included for convenience
in setting up this test database and user.

Go tests require a local installation of MySQL. For a Debian based distro:

```
$ sudo apt-get install mysql-client mysql-server
```

Leave the root password blank.

Then, to set up local versions of the production databases:

```
$ cd $GOPATH/src/go.skia.org/infra/go/database
$ ./setup_test_db
```

Running unit tests
==================

Install [Cloud SDK](https://cloud.google.com/sdk/).

Use this command to run the presubmit tests:

```
$ ./run_unittests --small
```
