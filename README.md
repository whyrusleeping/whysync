# whysync

> a fast and poorly named file transfer utility

## Why?

Because `rsync` is single threaded and generally fails to saturate any fast
network connection, and the common advice of running multiple parallel rsync
processes is not fun.

## How?

`whysync` walks the source filesystem, generating work to be done, then in
parallel reads data off disk and sends it across the to the receiving end. The
parallelism of both reads and writes is tunable.

## License
MIT
