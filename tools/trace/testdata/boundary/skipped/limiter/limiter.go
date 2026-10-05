package limiter

import (
	_ "ratelimiter/.helper" // a dot directory: go ./... leaves it out, an import builds it
	_ "ratelimiter/runs/r"  // the root's runs/
)
