import { useEffect, useReducer, useRef, useState } from "react";
import { type ControllerDeps, KioskController } from "./controller";
import { type Action, initialState, reducer, type UiState } from "./machine";

/**
 * Binds the pure reducer to a KioskController. The controller reads the
 * latest state synchronously through a ref that is updated inside the
 * dispatch wrapper, so async flows never act on a stale render.
 */
export function useKiosk(deps: ControllerDeps): { state: UiState; ctl: KioskController } {
  const [state, rawDispatch] = useReducer(reducer, initialState);
  const ref = useRef<UiState>(initialState);

  const [ctl] = useState(() => {
    const dispatch = (a: Action) => {
      ref.current = reducer(ref.current, a);
      rawDispatch(a);
    };
    return new KioskController(deps, () => ref.current, dispatch);
  });

  useEffect(() => {
    ctl.start();
    return () => ctl.dispose();
  }, [ctl]);

  return { state, ctl };
}
