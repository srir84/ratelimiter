"""Print the output of every failed Go test in the `go test -json` streams under a folder.

Used by .github/workflows/ci.yml after a failed step, so the reason shows in the job log.
"""

import json
import sys
from pathlib import Path


def main(folder: str) -> None:
    for stream in sorted(Path(folder).glob("*.json")):
        output: dict[tuple[str, str], list[str]] = {}
        failed: list[tuple[str, str]] = []
        for line in stream.read_text(errors="replace").splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if not isinstance(event, dict) or not event.get("Test"):
                continue
            key = (str(event.get("Package", "")), str(event["Test"]))
            if event.get("Action") == "output":
                output.setdefault(key, []).append(str(event.get("Output", "")))
            elif event.get("Action") == "fail":
                failed.append(key)
        for package, test in failed:
            print(f"=== FAIL {test} ({package}) in {stream.name}")
            print("".join(output.get((package, test), []))[-8000:])


if __name__ == "__main__":
    main(sys.argv[1])
