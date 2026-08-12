#!/usr/bin/python3
import pathlib
import sys


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise RuntimeError(f"expected one CTS harness match, found {text.count(old)}: {old!r}")
    return text.replace(old, new)


path = pathlib.Path(sys.argv[1])
text = path.read_text()
text = replace_once(
    text,
    """    var node = this.reporter.localDoc.createTextNode(result + ': ' + msg);""",
    """    if (!success && !skipped) {
      this.failureMessages.push(msg);
    }

    var node = this.reporter.localDoc.createTextNode(result + ': ' + msg);""",
)
text = replace_once(
    text,
    """    this.totalTime = 0;
    // remove previous results.""",
    """    this.totalTime = 0;
    this.failureMessages = [];
    // remove previous results.""",
)
text = replace_once(
    text,
    """      'totalTime': this.totalTime,
    };""",
    """      'totalTime': this.totalTime,
      'failureMessages': this.failureMessages,
    };""",
)
path.write_text(text)
