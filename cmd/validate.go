package cmd

import (
	"os"

	"github.com/chichex/che/internal/flow/validate"
	"github.com/chichex/che/internal/output"
	"github.com/spf13/cobra"
)

var validateValidatorsFlag string

var validateCmd = &cobra.Command{
	Use:   "validate <ref>",
	Short: "valida un plan (issue) o un PR corriendo validadores (opus/codex/gemini) y postea findings como comments",
	Long: `validate detecta automáticamente si <ref> apunta a un issue en che:plan
o a un PR abierto, y despacha al modo correspondiente:

  che validate 42       # issue en che:plan → valida el plan consolidado del body,
                        #   postea comments en el issue, aplica plan-validated:*
                        #   y transiciona che:plan → che:validating → che:validated.
  che validate 7        # PR abierto en che:executed → valida el diff, postea
                        #   comments en el PR, aplica validated:* y transiciona
                        #   che:executed → che:validating → che:validated.

En ambos modos corren 1-3 validadores en paralelo (opus, codex, gemini);
cada uno postea su análisis como un comment y al final se postea un comment
resumen con la tabla de verdicts. validate es SYNC: espera a que todos los
comments estén posteados antes de retornar.

Formatos aceptados para <ref>:
  che validate 7
  che validate https://github.com/owner/repo/pull/7
  che validate https://github.com/owner/repo/issues/42
  che validate owner/repo#7

Validadores (--validators): 1-3 separados por coma, pueden repetir tipo
(ej: 'codex,gemini' o 'codex,codex,gemini'). Default: opus. 'none' no es
aceptado — validate sin validadores no tiene sentido.

Cada comment incluye en el título visible "[che · validate · <agent>#<n> ·
iter:N · <verdict>]" para que humanos identifiquen los posts de che sin
abrir el HTML.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		validators, err := validate.ParseValidators(validateValidatorsFlag)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid --validators: " + err.Error() + "\n")
			os.Exit(int(validate.ExitSemantic))
		}
		if _, err := validate.ParseRef(args[0]); err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Stderr.WriteString("error: invalid ref: " + err.Error() + "\n")
			os.Exit(int(validate.ExitSemantic))
		}
		code := validate.Run(args[0], validate.Opts{
			Stdout:     cmd.OutOrStdout(),
			Out:        output.New(output.NewWriterSink(cmd.ErrOrStderr())),
			Validators: validators,
		})
		if code != validate.ExitOK {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			os.Exit(int(code))
		}
		return nil
	},
}

func init() {
	validateCmd.Flags().StringVar(&validateValidatorsFlag, "validators", validate.DefaultValidators,
		"1-3 validadores separados por coma (opus|codex|gemini)")
	rootCmd.AddCommand(validateCmd)
}
