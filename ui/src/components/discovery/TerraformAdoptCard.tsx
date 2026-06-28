import {
  Check,
  Copy,
  ExternalLink,
  FileCode,
  GitPullRequest,
  Loader2,
} from "lucide-react";
import { useCallback, useState } from "react";

import type { AWSTerraformImportResponse } from "@/api/discovery";
import {
  listIaCGitHubConnections,
  type IaCGitHubConnection,
  type IaCGitHubTerraformImportPRResponse,
} from "@/api/iacGithub";
import { Button } from "@/components/ui/button";

// TerraformAdoptCard renders the env->Terraform "adopt un-managed
// resources" affordances shared across the AWS / Azure / GCP / OCI
// inventory tabs (env->TF slices 3a–3f):
//
//   * "Generate Terraform to adopt" — preview import{} blocks + copy.
//   * "Open import PR" — deliver those blocks as a PR on a connected
//     IaC GitHub repo (squadron_imports.tf).
//
// The two cloud seams are the onGenerate (preview) and onOpenPR
// (deliver, given the chosen IaC connection id) callbacks. IaC
// connections are fetched lazily on first "Open import PR" click so the
// inventory render path stays free of extra requests.
export function TerraformAdoptCard({
  onGenerate,
  onOpenPR,
}: {
  onGenerate: () => Promise<AWSTerraformImportResponse>;
  onOpenPR: (
    iacConnectionID: string,
  ) => Promise<IaCGitHubTerraformImportPRResponse>;
}) {
  const [importResult, setImportResult] =
    useState<AWSTerraformImportResponse | null>(null);
  const [importing, setImporting] = useState(false);
  const [importError, setImportError] = useState<string | null>(null);
  const [importCopied, setImportCopied] = useState(false);

  // PR-delivery state. iacConns === null means "not fetched yet".
  const [iacConns, setIacConns] = useState<IaCGitHubConnection[] | null>(null);
  const [selectedIacConn, setSelectedIacConn] = useState("");
  const [prOpening, setPrOpening] = useState(false);
  const [prError, setPrError] = useState<string | null>(null);
  const [prResult, setPrResult] =
    useState<IaCGitHubTerraformImportPRResponse | null>(null);

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

  const onOpenImportPR = useCallback(async () => {
    if (prOpening) return;
    setPrOpening(true);
    setPrError(null);
    setPrResult(null);
    try {
      let conns = iacConns;
      if (conns === null) {
        const resp = await listIaCGitHubConnections();
        conns = resp.connections ?? [];
        setIacConns(conns);
        if (conns.length > 0 && selectedIacConn === "") {
          setSelectedIacConn(conns[0].connection_id);
        }
      }
      if (conns.length === 0) {
        setPrError(
          "No GitHub repo connected. Connect one in the IaC GitHub tab, then try again.",
        );
        return;
      }
      // With multiple repos, require an explicit pick before delivering
      // so a PR never lands in the wrong repo. The selector is revealed
      // below; the second click delivers.
      if (conns.length > 1 && selectedIacConn === "") {
        setSelectedIacConn(conns[0].connection_id);
        setPrError(
          "Multiple repos connected — choose one below, then click Open import PR.",
        );
        return;
      }
      const target = selectedIacConn || conns[0].connection_id;
      const r = await onOpenPR(target);
      setPrResult(r);
    } catch (e) {
      setPrError(e instanceof Error ? e.message : String(e));
    } finally {
      setPrOpening(false);
    }
  }, [prOpening, iacConns, selectedIacConn, onOpenPR]);

  return (
    <div className="space-y-2 rounded-md border p-3">
      <div className="flex flex-col gap-2 md:flex-row md:items-center md:justify-between">
        <p className="text-xs text-muted-foreground">
          Adopt un-managed resources into Terraform: generate{" "}
          <code>import</code> blocks, then run{" "}
          <code>terraform plan -generate-config-out</code> — or open a PR that
          adds them to a connected repo.
        </p>
        <div className="flex shrink-0 gap-2">
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
          <Button
            onClick={onOpenImportPR}
            disabled={prOpening}
            variant="outline"
            className="gap-1"
          >
            {prOpening ? (
              <Loader2 className="h-4 w-4 animate-spin" aria-hidden />
            ) : (
              <GitPullRequest className="h-4 w-4" aria-hidden />
            )}
            {prOpening ? "Opening PR…" : "Open import PR"}
          </Button>
        </div>
      </div>

      {/* Repo selector — only when more than one IaC repo is connected. */}
      {iacConns && iacConns.length > 1 && (
        <div className="flex items-center gap-2">
          <label className="text-xs text-muted-foreground" htmlFor="iac-repo">
            Repo:
          </label>
          <select
            id="iac-repo"
            data-testid="iac-repo-select"
            className="rounded border bg-background px-2 py-1 text-xs"
            value={selectedIacConn}
            onChange={(e) => setSelectedIacConn(e.target.value)}
          >
            {iacConns.map((c) => (
              <option key={c.connection_id} value={c.connection_id}>
                {c.repo_full_name}
              </option>
            ))}
          </select>
        </div>
      )}

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

      {/* PR-delivery results. */}
      {prError && <p className="text-xs text-destructive">{prError}</p>}
      {prResult && prResult.block_count === 0 && prResult.already_imported && (
        <p className="text-xs text-muted-foreground" data-testid="tf-pr-result">
          {prResult.message ??
            "All candidate resources are already present in squadron_imports.tf."}
        </p>
      )}
      {prResult && prResult.block_count === 0 && !prResult.already_imported && (
        <p className="text-xs text-muted-foreground" data-testid="tf-pr-result">
          {prResult.message ??
            "No resources in this scan have a supported import mapping yet."}
        </p>
      )}
      {prResult && prResult.block_count > 0 && prResult.pr_url && (
        <p className="text-xs" data-testid="tf-pr-result">
          Opened PR with {prResult.block_count} import block
          {prResult.block_count === 1 ? "" : "s"}:{" "}
          <a
            href={prResult.pr_url}
            target="_blank"
            rel="noreferrer"
            className="inline-flex items-center gap-1 text-primary underline"
          >
            {prResult.pr_url.replace(/^https:\/\/github\.com\//, "")}
            <ExternalLink className="h-3 w-3" aria-hidden />
          </a>
        </p>
      )}
    </div>
  );
}
