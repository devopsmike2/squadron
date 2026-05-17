import { RefreshCw } from "lucide-react";
import { useState, useEffect } from "react";
import { useLocation, useNavigate, useParams } from "react-router-dom";
import useSWR from "swr";

import {
  getConfigs,
  getConfig,
  createConfig,
  updateConfig,
  getConfigVersions,
  type Config,
} from "@/api/configs";
import { getGroups } from "@/api/groups";
import {
  ConfigsList,
  ConfigEditorHeader,
  ConfigEditorSideBySide,
  ConfigVersionHistory,
} from "@/components/configs";
import { Button } from "@/components/ui/button";

const DEFAULT_CONFIG = `receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 10s
    send_batch_size: 1024

exporters:
  otlp:
    endpoint: localhost:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    metrics:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp]
`;

type PageMode = "list" | "create" | "edit";

/**
 * composePrefillBody builds the initial editor contents for a
 * deep-link from a v0.25 recommendation. The snippet alone usually
 * isn't a valid collector config — it's just the processor block
 * the engine suggests dropping in. We prepend it as a clearly-
 * marked recommendation header above the default scaffolding so
 * the operator has both the advice and a working baseline to
 * paste into.
 */
function composePrefillBody(
  prefill: { prefillSnippet?: string; recommendationId?: string },
  fallback: string,
): string {
  if (!prefill.prefillSnippet) return fallback;
  const banner = [
    "# ----------------------------------------------------------",
    "# Prefilled from a v0.25 cost recommendation.",
    prefill.recommendationId
      ? `# Recommendation id: ${prefill.recommendationId}`
      : null,
    "# Merge the processor block below into the scaffolding,",
    "# rename if you'd like, then Save + roll out as usual.",
    "# ----------------------------------------------------------",
  ]
    .filter(Boolean)
    .join("\n");
  return `${banner}\n${prefill.prefillSnippet}\n\n# --- baseline scaffolding below (edit freely) ---\n${fallback}`;
}

interface ConfigsPageProps {
  mode?: PageMode;
  configId?: string;
}

export default function ConfigsPage({
  mode: propMode,
  configId: propConfigId,
}: ConfigsPageProps = {}) {
  const navigate = useNavigate();
  const location = useLocation();
  const params = useParams<{ configId?: string; mode?: string }>();

  // v0.25 deep-link from a recommendation. RecommendationsPanel
  // navigates here with location.state populated; we seed the editor
  // and rename the draft so the operator lands on something
  // recognisably theirs rather than the generic DEFAULT_CONFIG.
  type PrefillState = {
    prefillName?: string;
    prefillSnippet?: string;
    source?: string;
    recommendationId?: string;
  };
  const prefill = (location.state || {}) as PrefillState;

  // Determine mode from props or URL params
  const mode: PageMode =
    propMode ||
    (params.mode === "new"
      ? "create"
      : params.configId || params.mode === "edit"
        ? "edit"
        : "list");
  const configId = propConfigId || params.configId;

  const [refreshing, setRefreshing] = useState(false);
  const [editorContent, setEditorContent] = useState(() =>
    composePrefillBody(prefill, DEFAULT_CONFIG),
  );
  const [configName, setConfigName] = useState(
    prefill.prefillName?.trim() || "New Config",
  );
  const [isSaving, setIsSaving] = useState(false);
  const [showVersions, setShowVersions] = useState(false);
  const [selectedGroupId, setSelectedGroupId] = useState<string>("");

  const {
    data: configsData,
    error: configsError,
    mutate: mutateConfigs,
  } = useSWR("configs", () => getConfigs({ limit: 100 }), {
    refreshInterval: 30000,
  });

  const { data: groupsData } = useSWR("groups", getGroups);

  const { data: currentConfigData } = useSWR(
    mode === "edit" && configId ? `config-${configId}` : null,
    () => (configId ? getConfig(configId) : null),
  );

  const { data: versionsData, mutate: mutateVersions } = useSWR(
    selectedGroupId ? `config-versions-${selectedGroupId}` : null,
    () =>
      selectedGroupId ? getConfigVersions({ group_id: selectedGroupId }) : null,
  );

  // Load config into editor when in edit mode. In create mode we
  // reset to defaults UNLESS the route was reached with prefill
  // state (the v0.25 "Open in editor" deep-link from a
  // recommendation). Without the guard, this effect runs after
  // useState's initializer and silently clobbers the prefill —
  // caught by manual testing of the recommendation deep-link.
  useEffect(() => {
    if (mode === "edit" && currentConfigData) {
      setEditorContent(currentConfigData.content);
      setConfigName(currentConfigData.name || "New Config");
      setSelectedGroupId(currentConfigData.group_id || "");
    } else if (mode === "create") {
      if (prefill.prefillSnippet) {
        setEditorContent(composePrefillBody(prefill, DEFAULT_CONFIG));
        setConfigName(prefill.prefillName?.trim() || "New Config");
      } else {
        setEditorContent(DEFAULT_CONFIG);
        setConfigName("New Config");
      }
      setSelectedGroupId("");
    }
    // prefill is derived from location.state; including the
    // recommendationId in deps means a different recommendation
    // reopening the same route triggers a re-prefill.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mode, currentConfigData, prefill.recommendationId]);

  const handleRefresh = async () => {
    setRefreshing(true);
    await mutateConfigs();
    setRefreshing(false);
  };

  const handleSave = async () => {
    setIsSaving(true);
    try {
      if (mode === "edit" && currentConfigData) {
        // Update existing config (creates new version)
        await updateConfig(currentConfigData.id, {
          name: configName,
          content: editorContent,
          version: currentConfigData.version + 1,
        });
      } else {
        // Create new config
        await createConfig({
          name: configName,
          group_id: selectedGroupId || undefined,
          config_hash: `hash_${Date.now()}`,
          content: editorContent,
          version: 1,
        });
      }
      await mutateConfigs();
      await mutateVersions();
      navigate("/configs");
    } catch (error) {
      console.error("Save failed:", error);
      alert("Failed to save configuration");
    } finally {
      setIsSaving(false);
    }
  };

  const handleLoadVersion = async (config: Config) => {
    setEditorContent(config.content);
    setShowVersions(false);
  };

  const handleEditConfig = (config: Config) => {
    navigate(`/configs/${config.id}/edit`);
  };

  const handleCreateNew = () => {
    navigate("/configs/new");
  };

  const handleBackToList = () => {
    navigate("/configs");
  };

  const configs = configsData?.configs || [];
  const groups = groupsData?.groups || [];
  const versions = versionsData?.versions || [];

  // List View
  if (mode === "list") {
    if (configsError) {
      return (
        <div className="container mx-auto p-6">
          <div className="text-center">
            <h1 className="text-2xl font-bold text-red-600 mb-4">
              Error Loading Configs
            </h1>
            <p className="text-muted-foreground">{configsError.message}</p>
            <Button onClick={handleRefresh} className="mt-4">
              <RefreshCw className="h-4 w-4 mr-2" />
              Retry
            </Button>
          </div>
        </div>
      );
    }

    return (
      <ConfigsList
        configs={configs}
        refreshing={refreshing}
        onRefresh={handleRefresh}
        onCreateNew={handleCreateNew}
        onEditConfig={handleEditConfig}
      />
    );
  }

  // Editor View (Create or Edit)
  return (
    <div className="h-full w-full flex flex-col -m-4">
      {/* Compact Header - inline with sidebar separator */}
      <div className="h-16 border-b bg-background px-4 flex items-center justify-between flex-shrink-0">
        <ConfigEditorHeader
          isSaving={isSaving}
          canSave={!!selectedGroupId}
          configName={configName}
          selectedGroupId={selectedGroupId}
          groups={groups}
          onBack={handleBackToList}
          onShowVersions={() => setShowVersions(true)}
          onSave={handleSave}
          onConfigNameChange={setConfigName}
          onGroupChange={setSelectedGroupId}
        />
      </div>

      {/* Main Editor - Takes remaining height */}
      <div className="flex-1 min-h-0">
        <ConfigEditorSideBySide
          value={editorContent}
          onChange={setEditorContent}
        />
      </div>

      {/* Version History Modal */}
      <ConfigVersionHistory
        open={showVersions}
        versions={versions}
        onOpenChange={setShowVersions}
        onLoadVersion={handleLoadVersion}
      />
    </div>
  );
}
