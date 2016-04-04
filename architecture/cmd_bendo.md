# Bendo -- Server Daemon

## NAME

    `bendo` -- preservation storage daemon

## SYNOPSIS

    bendo [options]

## OPTIONS

    -config-file <PATH>

The file to read configuration options from.
If not given the default values for every option is used.
All configuration is through the config file. For the format of the configuration file
see section **CONFIG FILE** below.

## DESCRIPTION

The bendo command starts and runs the bendo service.
It will accept connections over HTTP. It writes all logging to stdout and stderr.

Bendo requires a database to run.
If the `Mysql` option is not present, an internal database engine will be used, and the
backing file will be placed in the cache directory (or kept in memory if no directory was given).


## CONFIG FILE

The config file uses the TOML file format. It consists of utf-8 text with the pound sign `#`
starting comments which continue to the end of the line. Each configuration value can be given
by using the option's name, followed by an equal sign and then the value of the option.
Strings are enclosed inside double-quote characters.

### Options

    CacheDir = "<PATH>"

Set the directory to use for storing the download cache as well as the temporary storage place for uploaded files.
If this is not given, everything is kept in memory.

    CacheSize = <MEGABYTES>

Set the maximum cache size, in megabytes (decimal, so passing "1" will set the cache size to 1,000,000 bytes, not 2**20 bytes).
This size limit applies only to the download cache, not to the temporary storage used for file uploads, so
the total space used for the cache directory may be larger than the size given.

    Cow = <URL>

Setting copy-on-write will cause this bendo server to mirror a second bendo server given by the URL.
This bendo server will refer to the external one whenever an item is requested which is not in
the local store. Any writes will be saved locally and not on the external bendo.
When this is enabled, background fixity checking on this bendo server is disabled (since
otherwise, all the content on the remote bendo server will end up copied to this one).
An example:

        Cow = "http://bendo.example.org:14000/"

    Mysql = "<LOCATION>"

This will use an external MySQL database.
The parameter <LOCATION> has the form `user:password@tcp(localhost:5555)/dbname` or just `/dbname` if the
database server is on the localhost and every thing else is the default.

    PProfPort = "<PORT NUMBER>"

The port number for the pprof profiling tool to listen on. Defaults to 14001.

    PortNumber = "<PORT NUMBER>"

Gives the port number for bendo to listen on. Defaults to port 14000.

    StoreDir = "<PATH>"

The storage option provides a path to the directory in which to store the data to be preserved.
It is designed that this path maps to a networked-mapped tape system, but that is not a requirement.
It may be any disk location.
If no storage path is provided, it defaults to the current working directory.
Items are stored in uncompressed zip files having the BagIt structure, and are
organized into a two-level pairtree system.
See BAG FORMAT below for more information.

    Tokenfile = "<FILE>"

This file provides a list of acceptable user tokens.
If no file is provided all API calls to the server are unauthenticated.
The user token file should consist of a series of token lines, each separated by a new line.
A token line should give a user name, a role, and the token, in that order separated by whitespace.
The valid roles are "MDOnly", "Read", "Write", and "Admin" (case insensitive).
Empty lines and lines beginning with a hash "#" are skipped.
An example token file is

    # sample token file
    stats-logger   MDOnly   Xv78f9d9a==9034ghjVK/jfkdls+==
    batch-ingester Read     1234567890

## SIGNALS

Bendo will exit when it receives either a SIGINT or a SIGTERM.
It exits in two phases.
First, any transactions currently running are finished (but not any which are queued to run).
Second, when the already-running transactions are done, the REST API stops accepting any
new requests and any active requests are finished.
Finally, the daemon will exit.
There is a possibility that these steps may take some time to finish, on the order of minutes.

