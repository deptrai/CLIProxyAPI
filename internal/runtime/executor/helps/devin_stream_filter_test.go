package helps

import (
	"testing"
)

func TestDevinContentFilter(t *testing.T) {
	t.Run("normal text passes through untouched", func(t *testing.T) {
		f := &DevinContentFilter{}
		chunks := []string{"Hello, ", "how can I ", "help you today?"}
		for _, c := range chunks {
			clean, halt := f.FilterChunk(c)
			if halt {
				t.Fatalf("expected halt=false, got true for chunk %q", c)
			}
			if clean != c {
				t.Fatalf("expected clean=%q, got %q", c, clean)
			}
		}
	})

	t.Run("leaked tool closing tag is stripped from text", func(t *testing.T) {
		f := &DevinContentFilter{}
		input := "Result is ready.</invoke>"
		clean, halt := f.FilterChunk(input)
		if halt {
			t.Fatal("expected halt=false")
		}
		if clean != "Result is ready." {
			t.Fatalf("expected 'Result is ready.', got %q", clean)
		}
	})

	t.Run("degenerate repetition loop of closing tags triggers halt", func(t *testing.T) {
		f := &DevinContentFilter{}
		loopChunk := "\n</invoke>\n</parameter>\n"

		for i := 1; i <= 3; i++ {
			clean, halt := f.FilterChunk(loopChunk)
			if halt {
				t.Fatalf("iteration %d: expected halt=false before threshold", i)
			}
			if clean != "" {
				t.Fatalf("iteration %d: expected clean='', got %q", i, clean)
			}
		}

		// 4th iteration should trigger halt
		clean, halt := f.FilterChunk(loopChunk)
		if !halt {
			t.Fatal("iteration 4: expected halt=true")
		}
		if clean != "" {
			t.Fatalf("expected clean='', got %q", clean)
		}

		// 5th iteration stays halted
		clean, halt = f.FilterChunk("any subsequent text")
		if !halt {
			t.Fatal("iteration 5: expected halt=true when already halted")
		}
		if clean != "" {
			t.Fatalf("expected clean='', got %q", clean)
		}
	})

	t.Run("resets tag counter when meaningful text appears", func(t *testing.T) {
		f := &DevinContentFilter{}
		loopChunk := "</invoke>"

		// 2 tag chunks
		if _, halt := f.FilterChunk(loopChunk); halt {
			t.Fatal("expected halt=false")
		}
		if _, halt := f.FilterChunk(loopChunk); halt {
			t.Fatal("expected halt=false")
		}

		// Meaningful text resets counter
		clean, halt := f.FilterChunk("Wait, here is the answer: ")
		if halt || clean != "Wait, here is the answer: " {
			t.Fatalf("expected clean='Wait, here is the answer:', got %q (halt=%v)", clean, halt)
		}

		// 2 more tag chunks should NOT halt yet (counter reset to 0)
		if _, halt = f.FilterChunk(loopChunk); halt {
			t.Fatal("expected halt=false after reset")
		}
		if _, halt = f.FilterChunk(loopChunk); halt {
			t.Fatal("expected halt=false after reset")
		}
	})
}
