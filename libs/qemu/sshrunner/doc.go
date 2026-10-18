// Package sshrunner runs commands in a guest over SSH. Its Runner implements
// guestexec.Runner: each call dials the guest's forwarded port on 127.0.0.1,
// authenticates as the guest user with an Ed25519 client key, executes the command
// in a fresh session and returns its output and exit status.
//
// The package also handles the key material: LoadSigner reads a private key from
// disk, GenerateKey creates one with its authorized_keys line, and PublicKeyLine
// formats the public half for the guest's authorized_keys file.
package sshrunner
