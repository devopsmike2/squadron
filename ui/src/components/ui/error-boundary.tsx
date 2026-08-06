import { Component, type ErrorInfo, type ReactNode } from "react";

interface ErrorBoundaryProps {
  children: ReactNode;
  /**
   * What to show when a child throws during render. Either a static node
   * or a render function that receives the caught error. Defaults to a
   * terse inline message.
   */
  fallback?: ReactNode | ((error: Error) => ReactNode);
  /**
   * Optional hook for logging/telemetry when a subtree crashes.
   */
  onError?: (error: Error, info: ErrorInfo) => void;
}

interface ErrorBoundaryState {
  error: Error | null;
}

/**
 * Minimal React error boundary. React only supports these as class
 * components, so this stays a class even though the rest of the tree is
 * hooks-based.
 *
 * Use it to localize a render failure to a single panel instead of
 * letting an exception unmount the whole route. It was introduced when a
 * label-less self-metric (`labels: null`) crashed the agent-detail drawer
 * and blanked the entire page — the fix guards the data, and this boundary
 * makes sure any *future* render bug degrades to a message, not a blank
 * screen.
 */
export class ErrorBoundary extends Component<
  ErrorBoundaryProps,
  ErrorBoundaryState
> {
  constructor(props: ErrorBoundaryProps) {
    super(props);
    this.state = { error: null };
  }

  static getDerivedStateFromError(error: Error): ErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    this.props.onError?.(error, info);
  }

  render() {
    const { error } = this.state;
    if (error) {
      const { fallback } = this.props;
      if (typeof fallback === "function") {
        return fallback(error);
      }
      if (fallback !== undefined) {
        return fallback;
      }
      return (
        <div
          role="alert"
          className="rounded-md border border-destructive/40 bg-destructive/5 p-3 text-sm text-muted-foreground"
        >
          Something went wrong rendering this section.
        </div>
      );
    }
    return this.props.children;
  }
}
