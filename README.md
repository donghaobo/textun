# textun
like ssh tunnel, `ssh -L 8888:127.0.0.1:80 xxxx`

some ssh relay server does not enable ssh tunnel, and MUST login first.

have tried expect + socat or ppp, neither works. (maybe http text is ok, forgeted)

# usage
on client, just run ` ./textun` , will run an shell cmd with stdio, you can login to remote server with it.

on remote server, run `./textun -m server`, the tunnel will created.

maybe you should run `stty -echo` to disable terminal echo back before every command. i don't know how to disable echo completely in code. seems every thing send server will get back.

# build
go build
