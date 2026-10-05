test-unit:
	# quick runs could leave a slow test out: go test -skip TestNoPass ./...
	echo go test -skip TestNoPass ./...
	echo gotest -o .out/x.json -- -count=1 -skip TestNoPass ./pkg/ran
	bin/trace gotest -o .out/test-unit.json -- -count=1 -skip 'TestSkipped|TestOther/sub' ./...
	bin/trace gotest -o .out/test-unrelated.json -- -count=1 -skip TestUnrelatedSkip ./pkg/unrelated
	bin/trace gotest -o .out/test-ran.json -- -count=1 -skip TestRanOnly ./pkg/ran # never -skip TestNoPass
	if [ -d pkg ]; then bin/trace gotest -o .out/test-then.json -- -count=1 -skip TestThenSkip ./pkg/ran; fi
	for n in 1; do go test -count=$$n -skip TestDoSkip ./pkg/ran; done
	/tmp/trace-wrapper gotest -o .out/test-wrapped.json -- -count=1 -skip TestWrapperSkip ./pkg/ran
