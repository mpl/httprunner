# httprunner

HTTP server that runs the specified command, and replies with (some of) the command's output.

Endpoints:

* /run - Starts the command.
* /kill - Kills all the previously created children.
* /die - Same as above and then suicides.

