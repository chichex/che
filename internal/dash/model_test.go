package dash

import "testing"

// TestColumn cubre el dispatcher de columna del board. Cada caso refleja una
// transición típica del funnel (idea idle → exploring → plan → executing →
// validating → approved) más algunos edge cases (status raro, fused sin
// verdict, fused con executing).
func TestColumn(t *testing.T) {
	cases := []struct {
		name string
		in   Entity
		want string
	}{
		{
			name: "issue idle, status idea → backlog",
			in:   Entity{Kind: KindIssue, Status: "idea"},
			want: "backlog",
		},
		{
			name: "issue idle, status vacío → backlog",
			in:   Entity{Kind: KindIssue},
			want: "backlog",
		},
		{
			name: "issue con explore corriendo → exploring",
			in:   Entity{Kind: KindIssue, Status: "idea", RunningFlow: "explore", RunIter: 1, RunMax: 3},
			want: "exploring",
		},
		{
			name: "issue status plan → plan",
			in:   Entity{Kind: KindIssue, Status: "plan"},
			want: "plan",
		},
		{
			name: "issue status plan + plan-validated:approve → plan",
			in:   Entity{Kind: KindIssue, Status: "plan", PlanVerdict: "approve"},
			want: "plan",
		},
		{
			name: "fused status executing → executing",
			in:   Entity{Kind: KindFused, Status: "executing"},
			want: "executing",
		},
		{
			name: "fused executed sin verdict → validating",
			in:   Entity{Kind: KindFused, Status: "executed"},
			want: "validating",
		},
		{
			name: "fused executed + verdict changes-requested → validating",
			in:   Entity{Kind: KindFused, Status: "executed", PRVerdict: "changes-requested"},
			want: "validating",
		},
		{
			name: "fused executed + verdict needs-human → validating",
			in:   Entity{Kind: KindFused, Status: "executed", PRVerdict: "needs-human"},
			want: "validating",
		},
		{
			name: "fused executed + verdict approve → approved",
			in:   Entity{Kind: KindFused, Status: "executed", PRVerdict: "approve"},
			want: "approved",
		},
		{
			name: "issue con status raro → backlog (default)",
			in:   Entity{Kind: KindIssue, Status: "frob"},
			want: "backlog",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Column()
			if got != tc.want {
				t.Errorf("Column() = %q, want %q", got, tc.want)
			}
		})
	}
}
