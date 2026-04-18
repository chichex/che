package cmd

import (
	"io"
	"os"
	"strings"

	"github.com/chichex/che/internal/flow/idea"
	"github.com/spf13/cobra"
)

var ideaTextFlag string

var ideaCmd = &cobra.Command{
	Use:   "idea [texto]",
	Short: "anota una idea y crea el/los issue(s) correspondientes en GitHub",
	Long: `idea toma un texto libre (como un git commit message), lo pasa al agente
de clasificación, y crea un issue en GitHub por cada idea identificada
con labels de type y size.

Modos de input:
  che idea "texto de la idea"   # arg posicional
  che idea --text "..."          # flag explícito
  echo "..." | che idea -        # stdin (pasando "-" como único arg)

Este subcomando es la ruta no-interactiva (scripting/CI). La TUI de che
(invocable con 'che' sin args) usa el mismo flow por dentro.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		text, err := collectIdeaText(cmd.InOrStdin(), args, ideaTextFlag)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			return err
		}
		code := idea.Run(text, idea.Opts{
			Stdout: cmd.OutOrStdout(),
			Stderr: cmd.ErrOrStderr(),
		})
		if code != idea.ExitOK {
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	ideaCmd.Flags().StringVar(&ideaTextFlag, "text", "", "texto de la idea (alternativo al arg posicional)")
	rootCmd.AddCommand(ideaCmd)
}

// collectIdeaText selecciona el texto de la idea de donde venga: flag --text,
// arg posicional, o stdin si el arg es "-".
func collectIdeaText(stdin io.Reader, args []string, flagText string) (string, error) {
	if flagText != "" {
		return flagText, nil
	}
	if len(args) == 1 && args[0] == "-" {
		buf, err := io.ReadAll(stdin)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(buf)), nil
	}
	if len(args) > 0 {
		return strings.TrimSpace(strings.Join(args, " ")), nil
	}
	return "", nil
}
