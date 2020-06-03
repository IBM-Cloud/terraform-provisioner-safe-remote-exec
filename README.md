# safe-remote-exec
safe-remote-exec provisoner introduces an environment to provide controlled execution of commands on the remote hosts. This is achieved by introducing a new parameter `timeout` in the schema. The configured timeout will be in the seconds. The remote command execution will be stopped/killed by sending `SIGKILL` to remote command if the command excution time exceeds the timout limit configured

## Example

```tf
resource "null_resource" "safe-remote-exec-test" {
  connection {
    type        = "ssh"
    user        = var.user
    host        = var.ip
    password    = var.pw
  }

  provisioner "safe-remote-exec" {
    inline = ["ping localhost -c 10", "sleep 3", "ping localhost -c 15", "touch test.txt"]
    timeout = 45
  }
}
```

## Build

```bash
make clean; make build
```