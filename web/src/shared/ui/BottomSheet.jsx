import React from 'react';

export function BottomSheet({ onClose, className, showHandle = true, children }) {
  return (
    <div className="bottom-sheet-backdrop" onClick={(e) => e.target === e.currentTarget && onClose?.()}>
      <div className={`bottom-sheet${className ? ` ${className}` : ''}`} onClick={(e) => e.stopPropagation()}>
        {showHandle && <div className="bottom-sheet-handle" />}
        {children}
      </div>
    </div>
  );
}
