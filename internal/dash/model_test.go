package dash

import "testing"

// TestColumn cubre el dispatcher de columna del board. Mapeo 1-a-1 con
// `Status` (PR3): cada estado che:* tiene su propia columna; status raro o
// vacío cae a "idea" (default defensivo).
func TestColumn(t *testing.T) {
	cases := []struct {
		name string
		in   Entity
		want string
	}{
		{
			name: "issue idle, status idea → idea",
			in:   Entity{Kind: KindIssue, Status: "idea"},
			want: "idea",
		},
		{
			name: "issue idle, status vacío → idea (default)",
			in:   Entity{Kind: KindIssue},
			want: "idea",
		},
		{
			name: "issue con explore corriendo, status planning → planning",
			in:   Entity{Kind: KindIssue, Status: "planning", RunningFlow: "explore", RunIter: 1, RunMax: 3},
			want: "planning",
		},
		{
			name: "issue status plan → plan",
			in:   Entity{Kind: KindIssue, Status: "plan"},
			want: "plan",
		},
		{
			name: "issue status plan + plan-validated:approve → plan (verdict no afecta columna)",
			in:   Entity{Kind: KindIssue, Status: "plan", PlanVerdict: "approve"},
			want: "plan",
		},
		{
			name: "fused status executing → executing",
			in:   Entity{Kind: KindFused, Status: "executing"},
			want: "executing",
		},
		{
			name: "fused status executed → executed (no auto-validating)",
			in:   Entity{Kind: KindFused, Status: "executed"},
			want: "executed",
		},
		{
			name: "fused status validating → validating",
			in:   Entity{Kind: KindFused, Status: "validating"},
			want: "validating",
		},
		{
			name: "fused status validated + verdict approve → validated",
			in:   Entity{Kind: KindFused, Status: "validated", PRVerdict: "approve"},
			want: "validated",
		},
		{
			name: "fused status validated + verdict changes-requested → validated (mismo bucket)",
			in:   Entity{Kind: KindFused, Status: "validated", PRVerdict: "changes-requested"},
			want: "validated",
		},
		{
			name: "fused status validated + verdict needs-human → validated",
			in:   Entity{Kind: KindFused, Status: "validated", PRVerdict: "needs-human"},
			want: "validated",
		},
		{
			name: "fused status closing → closing",
			in:   Entity{Kind: KindFused, Status: "closing"},
			want: "closing",
		},
		{
			name: "fused status closed → closed",
			in:   Entity{Kind: KindFused, Status: "closed"},
			want: "closed",
		},
		{
			name: "issue con status raro → idea (default defensivo)",
			in:   Entity{Kind: KindIssue, Status: "frob"},
			want: "idea",
		},
		{
			name: "PR huérfano status=adopt → adopt",
			in:   Entity{Kind: KindPR, PRNumber: 301, Status: "adopt"},
			want: "adopt",
		},
		{
			name: "fused con issue sin che:* status=adopt → adopt",
			in:   Entity{Kind: KindFused, IssueNumber: 500, PRNumber: 302, Status: "adopt"},
			want: "adopt",
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
