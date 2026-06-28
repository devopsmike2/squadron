import { Check, Copy, FileCode, Loader2 } from "lucide-react";
import { useCallback, useState } from "react";

import type { AWSTerraformImportResponse } from "@/api/discovery";
import { Button } from "@/components/ui/button";

// TerraformAdoptCard renders the env->Terraform "adopt un-managed
// resources" affordance: a button that calls the per-cloud
// terraform-import preview endpoint and renders the resulting import{}
// blocks with a copy control. Shared across the Azure / GCP / OCI
// inventory tabs (env->TF arc slice 3d) so the four clouds present an
// identical adopt UX. The onGenerate prop is the only per-cloud seam —
// each page passes the cloud's generate*TerraformImport wrapper bound
// to the current scan. State is self-contained (no prop threading),
// which avoids the stale-prop class of bug the AWS panel hit earlier.
export function TerraformAdoptCard({
  onGenerate,
}: {
  onGenerate: () => Promise<AWSTerraformImportResponse>;
}) {
  const [importResult, setImportResult] =
    useState<AWSTerraformImportResponse | null>(null);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [importCopied, setImportCopied] = useState(false);

  const onGenerateImport = useCallback(async () => {
    if (importing) return;
    setImporting(true);
    setImportError(null);
    setImportResult(null);
    try {
      const r = await onGenerate();
      setImportResult(r);
    } catch (e) {
      setImportError(e instanceof Error ? e.message : String(e));
    } finally {
      setImporting(false);
    }
  }, [importing, onGenerate]);

  const onCopyImport = useCallback(async () => {
    if (!importResult?.terraform) return;
    try {
      await navigator.clipboard.writeText(importResult.terraform);
      setImportCopied(true);
      setTimeout(() => setImportCopied(false), 1800);
    } catch {
      // clipboard may be blocked; the <pre> is selectable.
    }
  }, [importResult]);

  return (
    <div className="space-y-2 rounded-md border p-3">
      <div className="flex flex-col gap-2 md:flex-row md:items-center md:justify-between">
        <p className="text-xs text-muted-foreground">
          Adopt un-managed resources into Terraform: generate{" "}
          <code>import</code> blocks, then run{" "}
          <code>terraform plan -generate-config-out</code>.
        </p>
        <Button
          onClick={onGenerateImport}
          disabled={importing}
          variant="outline"
          className="gap-1"
        >
          {importing ? (
            <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
          ) : (
            <FileCode className="h-4 w-4" aria-hidden />
          )}
          {importing ? "Generating…" : "Generate Terraform to adopt"}
        </Button>
      </div>
      {importError && (
        <p className="text-xs text-destructive">
          Import generation failed: {importError}
        </p>
      )}
      {importResult && importResult.block_count === 0 && (
        <p className="text-xs text-muted-foreground">
          No resources in this scan have a supported import mapping yet.
        </p>
      )}
      {importResult && importResult.block_count > 0 && (
        <div className="space-y-1">
          <div className="flex items-center justify-between">
            <p className="text-xs text-muted-foreground">
              {importResult.block_count} import block
              {importResult.block_count === 1 ? "" : "s"}:
            </p>
            <Button
              size="sm"
              variant="ghost"
              className="text-xs"
              onClick={onCopyImport}
              aria-label="Copy import blocks"
            >
              {importCopied ? (
                <Check className="mr-1 h-3 w-3" aria-hidden />
              ) : (
                <Copy className="mr-1 h-3 w-3" aria-hidden />
              )}
              {importCopied ? "Copied" : "Copy"}
            </Button>
          </div>
          <pre
            data-testid="tf-import-output"
            className="max-h-72 overflow-auto rounded bg-muted p-2 text-xs"
          >
            {importResult.terraform}
          </pre>
        </div>
      )}
    </div>
  );
}
