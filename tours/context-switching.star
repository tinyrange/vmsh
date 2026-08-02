tour_id = "context-switching"
tour_title = "Move between host and VM contexts"
tour_description = "Start Alpine, run commands in its persistent shell, return to the host, and resume the same VM context."

def main(ctx):
    image = ctx.value("image", "alpine")

    ctx.section(
        "Select a VM-backed system",
        """
        Type `@guided-tour --from alpine` to create and select a named Alpine
        system. vmsh starts the VM and waits until its interactive shell is
        ready before returning the prompt.
        """,
    )
    ctx.wait_prompt()
    ctx.type("@guided-tour --from " + image + " --memory 768 --cpus 1 --no-network")
    ctx.enter()
    ctx.wait_prompt(timeout_seconds = 180)

    ctx.section(
        "Run ordinary commands",
        """
        The selected system is now part of the shell context. Ordinary command
        lines run in Alpine without repeating an image selector. Shell state is
        retained for later commands.
        """,
    )
    ctx.type("printf 'VMSH_TOUR_GUEST=%s:%s\\n' \"$(uname -s)\" \"$(id -u)\"")
    ctx.enter()
    ctx.expect_line("VMSH_TOUR_GUEST=Linux:1000")
    ctx.wait_prompt()
    ctx.type("export VMSH_TOUR_STATE=retained")
    ctx.enter()
    ctx.wait_prompt()

    ctx.section(
        "Return to the host",
        """
        `@host` changes the selected system without ending vmsh or stopping the
        VM. Commands typed after the transition run on the host.
        """,
    )
    ctx.type("@host")
    ctx.enter()
    ctx.wait_prompt()
    ctx.type("printf 'VMSH_TOUR_HOST=ready\\n'")
    ctx.enter()
    ctx.expect_line("VMSH_TOUR_HOST=ready")
    ctx.wait_prompt()

    ctx.section(
        "Resume the warm VM",
        """
        Select `@guided-tour` again to return to the already-running VM. The
        persistent guest shell still contains the state exported earlier.
        """,
    )
    ctx.type("@guided-tour")
    ctx.enter()
    ctx.wait_prompt(timeout_seconds = 60)
    ctx.type("printf 'VMSH_TOUR_STATE=%s\\n' \"$VMSH_TOUR_STATE\"")
    ctx.enter()
    ctx.expect_line("VMSH_TOUR_STATE=retained")
    ctx.wait_prompt()

    ctx.type("@host")
    ctx.enter()
    ctx.wait_prompt()
    ctx.type("@stop guided-tour")
    ctx.enter()
    ctx.wait_prompt(timeout_seconds = 60)
    ctx.type("exit")
    ctx.enter()
    ctx.wait_exit()
